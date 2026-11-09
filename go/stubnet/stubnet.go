// Package stubnet is an [http.RoundTripper] for tests and dry runs. Requests
// to hosts registered with [Transport.Handle] are answered in process by an
// [http.Handler]; every other destination is refused and recorded, so nothing
// can leave the process by accident — including code that uses
// http.DefaultClient, once [Transport.InstallDefault] has been called.
//
// The adapters under test run unchanged: only the transport differs. The
// transport behaves like a real one in the ways that matter for sending side
// effects safely:
//
//   - Before a handler runs, the request's [httptrace.ClientTrace] hooks
//     WroteHeaders and WroteRequest are called, so code that decides "was
//     the request sent?" from them (see the httpsent package) sees the truth.
//     Refused destinations and [Refuse] faults fail before those hooks, like
//     a connection that could not be opened.
//   - [Transport.Inject] queues failures for the next requests to a host:
//     [Refuse] (never sent), [Lose] (the handler runs — the server acted —
//     but the response is lost) and [Status] (an error status without
//     calling the handler). They reproduce the cases an at-most-once sender
//     must handle.
//   - Every exchange is recorded in a bounded journal with the method, host
//     and path only. Query strings and headers are never recorded: they may
//     carry tokens.
package stubnet

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

// Sentinel errors. Use [errors.Is] to test for them.
var (
	// ErrBlocked: the destination is not registered. Nothing was sent.
	ErrBlocked = errors.New("stubnet: destination not registered")
	// ErrRefused: an injected [Refuse] fault. Nothing was sent.
	ErrRefused = errors.New("stubnet: connection refused (injected)")
	// ErrLost: an injected [Lose] fault. The handler received the request
	// (the server acted) but the response never arrived.
	ErrLost = errors.New("stubnet: response lost after the request was delivered (injected)")
)

// Fault makes one request fail on purpose. Use [Refuse], [Lose] or [Status].
type Fault struct {
	kind   string
	status int
}

var (
	// Refuse fails the request before it is written (a refused connection).
	Refuse = Fault{kind: "refuse"}
	// Lose delivers the request to the handler, then drops the response.
	Lose = Fault{kind: "lose"}
)

// Status answers the request with the given HTTP status (and an empty body)
// without calling the handler. The request counts as delivered.
func Status(code int) Fault { return Fault{kind: "status", status: code} }

func (f Fault) String() string {
	if f.kind == "status" {
		return fmt.Sprintf("status %d", f.status)
	}
	return f.kind
}

// Entry is one recorded request.
type Entry struct {
	At     time.Time
	Method string
	Host   string
	Path   string // without the query string
	Status int    // 0 when no response was returned
	// Fault is the injected fault ("" for none), Blocked reports a refused
	// destination, and Err is the error returned to the client, if any.
	Fault   string
	Blocked bool
	Err     string
}

// DefaultMaxEntries bounds the journal when [Transport.MaxEntries] is 0.
const DefaultMaxEntries = 1000

// Transport is the stub network. The zero value is not usable; call [New].
// It is safe for concurrent use.
type Transport struct {
	// Now stamps journal entries (time.Now when nil).
	Now func() time.Time
	// MaxEntries bounds the journal (oldest dropped); 0 = DefaultMaxEntries.
	MaxEntries int

	mu      sync.Mutex
	hosts   map[string]http.Handler
	faults  map[string][]Fault
	journal []Entry
}

var _ http.RoundTripper = (*Transport)(nil)

// New returns a transport that refuses every destination until hosts are
// registered with [Transport.Handle].
func New() *Transport {
	return &Transport{hosts: map[string]http.Handler{}, faults: map[string][]Fault{}}
}

// Handle answers requests to host (a host name such as "api.example.test",
// matched case-insensitively and regardless of port) with h.
func (t *Transport) Handle(host string, h http.Handler) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.hosts[strings.ToLower(host)] = h
}

// Inject makes the next request to host fail with f. Several faults queue up
// and are used in order, one per request.
func (t *Transport) Inject(host string, f Fault) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h := strings.ToLower(host)
	t.faults[h] = append(t.faults[h], f)
}

// Client returns an HTTP client that uses t.
func (t *Transport) Client() *http.Client { return &http.Client{Transport: t} }

// InstallDefault makes t the process-wide [http.DefaultTransport] (used by
// http.DefaultClient and by clients without a Transport) and returns a
// function that restores the previous one. Tests that call it must not run
// in parallel with tests that need the network.
func (t *Transport) InstallDefault() (restore func()) {
	prev := http.DefaultTransport
	http.DefaultTransport = t
	return func() { http.DefaultTransport = prev }
}

// Journal returns a copy of the recorded requests, oldest first.
func (t *Transport) Journal() []Entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Entry(nil), t.journal...)
}

// Blocked returns the requests to unregistered destinations, oldest first.
func (t *Transport) Blocked() []Entry {
	var out []Entry
	for _, e := range t.Journal() {
		if e.Blocked {
			out = append(out, e)
		}
	}
	return out
}

func (t *Transport) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Transport) record(e Entry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e.At = t.now()
	t.journal = append(t.journal, e)
	limit := t.MaxEntries
	if limit <= 0 {
		limit = DefaultMaxEntries
	}
	if over := len(t.journal) - limit; over > 0 {
		t.journal = append([]Entry(nil), t.journal[over:]...)
	}
}

// route returns the handler for host and takes the next queued fault.
func (t *Transport) route(host string) (http.Handler, Fault, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	h, ok := t.hosts[host]
	if !ok {
		return nil, Fault{}, false
	}
	var f Fault
	if q := t.faults[host]; len(q) > 0 {
		f, t.faults[host] = q[0], q[1:]
	}
	return h, f, true
}

// RoundTrip answers req from the registered handler, or fails as described
// in the package documentation. Like every RoundTripper it closes the
// request body.
func (t *Transport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	if req.Body != nil {
		defer req.Body.Close()
	}
	host := strings.ToLower(req.URL.Hostname())
	e := Entry{Method: req.Method, Host: host, Path: req.URL.Path}
	defer func() {
		if resp != nil {
			e.Status = resp.StatusCode
		}
		if err != nil {
			e.Err = err.Error()
		}
		t.record(e)
	}()

	h, fault, ok := t.route(host)
	if !ok {
		e.Blocked = true
		return nil, fmt.Errorf("%w: %s %s", ErrBlocked, req.Method, host)
	}
	e.Fault = fault.kind
	if fault.kind == "status" {
		e.Fault = fault.String()
	}
	if err := req.Context().Err(); err != nil {
		return nil, err // cancelled before anything was written
	}
	if fault == Refuse {
		return nil, ErrRefused
	}

	// From here on the request has "reached the server".
	if tr := httptrace.ContextClientTrace(req.Context()); tr != nil {
		if tr.WroteHeaders != nil {
			tr.WroteHeaders()
		}
		if tr.WroteRequest != nil {
			tr.WroteRequest(httptrace.WroteRequestInfo{})
		}
	}
	if fault.kind == "status" {
		return response(req, fault.status, nil), nil
	}
	rec, err := serve(h, req)
	if err != nil {
		return nil, err
	}
	if fault == Lose {
		return nil, ErrLost
	}
	if err := req.Context().Err(); err != nil {
		return nil, err // the client gave up while the server was working
	}
	resp = rec.Result()
	resp.Request = req
	return resp, nil
}

// serve runs the handler on a server-side copy of req. A panicking handler is
// reported like a connection closed by the server (after the request was
// delivered).
func serve(h http.Handler, req *http.Request) (rec *httptest.ResponseRecorder, err error) {
	sreq := req.Clone(req.Context())
	sreq.RequestURI = req.URL.RequestURI()
	sreq.RemoteAddr = "192.0.2.1:49152" // TEST-NET-1: never a real peer
	if sreq.Host == "" {
		sreq.Host = req.URL.Host
	}
	if req.Body == nil {
		sreq.Body = http.NoBody
	} else {
		// Read the body here so the handler cannot outlive RoundTrip with it.
		b, rerr := io.ReadAll(req.Body)
		if rerr != nil {
			return nil, rerr
		}
		sreq.Body = io.NopCloser(bytes.NewReader(b))
	}
	rec = httptest.NewRecorder()
	defer func() {
		if p := recover(); p != nil {
			rec, err = nil, fmt.Errorf("stubnet: handler panicked after the request was delivered: %v", p)
		}
	}()
	h.ServeHTTP(rec, sreq)
	return rec, nil
}

func response(req *http.Request, status int, body []byte) *http.Response {
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
