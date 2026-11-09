package stubnet_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/caprico99278/FledgeFlow-oss/go/httpsent"
	"github.com/caprico99278/FledgeFlow-oss/go/sendguard"
	"github.com/caprico99278/FledgeFlow-oss/go/stubnet"
)

const host = "api.example.test"

// postsAPI is a tiny stand-in for an external API: POST /posts creates a post.
type postsAPI struct {
	created  atomic.Int64
	lastBody atomic.Value
}

func (a *postsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/posts" {
		http.NotFound(w, r)
		return
	}
	b, _ := io.ReadAll(r.Body)
	a.lastBody.Store(string(b))
	n := a.created.Add(1)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = fmt.Fprintf(w, `{"id":"post-%d"}`, n)
}

func newNet(t *testing.T) (*stubnet.Transport, *postsAPI) {
	t.Helper()
	api := &postsAPI{}
	tr := stubnet.New()
	tr.Handle(host, api)
	return tr, api
}

func postReq(t *testing.T, ctx context.Context, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestRegisteredHostIsServed(t *testing.T) {
	tr, api := newNet(t)
	resp, sent, err := httpsent.Do(tr.Client(), postReq(t, context.Background(), "https://API.example.test:8443/posts?access_token=secret-value"))
	if err != nil || !sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated || string(b) != `{"id":"post-1"}` || resp.Request == nil {
		t.Fatalf("response %d %s", resp.StatusCode, b)
	}
	if api.lastBody.Load() != `{"text":"hello"}` {
		t.Fatalf("handler saw body %v", api.lastBody.Load())
	}
	j := tr.Journal()
	if len(j) != 1 || j[0].Host != host || j[0].Path != "/posts" || j[0].Status != http.StatusCreated || j[0].Blocked {
		t.Fatalf("journal %+v", j)
	}
	// Query strings (tokens) are never recorded.
	if strings.Contains(fmt.Sprintf("%+v", j), "secret-value") {
		t.Fatal("the journal recorded the query string")
	}
}

// Anything not registered is refused before sending, and recorded.
func TestUnregisteredHostIsBlocked(t *testing.T) {
	tr, _ := newNet(t)
	_, sent, err := httpsent.Do(tr.Client(), postReq(t, context.Background(), "https://elsewhere.example.test/posts"))
	if !errors.Is(err, stubnet.ErrBlocked) || sent {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
	b := tr.Blocked()
	if len(b) != 1 || b[0].Host != "elsewhere.example.test" || b[0].Method != http.MethodPost {
		t.Fatalf("blocked %+v", b)
	}
}

func TestInstallDefaultCoversDefaultClient(t *testing.T) {
	tr, _ := newNet(t)
	restore := tr.InstallDefault()
	_, err := http.Get("https://unregistered.example.test/")
	restore()
	if !errors.Is(err, stubnet.ErrBlocked) {
		t.Fatalf("http.Get through the default transport: %v", err)
	}
	if http.DefaultTransport == tr {
		t.Fatal("restore did not put the previous transport back")
	}
}

func TestFaults(t *testing.T) {
	cases := []struct {
		name        string
		fault       stubnet.Fault
		wantSent    bool
		wantCreated int64
		wantStatus  int
		wantErr     error
	}{
		{"refuse: never reaches the server", stubnet.Refuse, false, 0, 0, stubnet.ErrRefused},
		{"lose: the server acted, the answer is lost", stubnet.Lose, true, 1, 0, stubnet.ErrLost},
		{"status: answered without the handler", stubnet.Status(http.StatusServiceUnavailable), true, 0, http.StatusServiceUnavailable, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, api := newNet(t)
			tr.Inject(host, c.fault)
			resp, sent, err := httpsent.Do(tr.Client(), postReq(t, context.Background(), "https://"+host+"/posts"))
			if sent != c.wantSent || api.created.Load() != c.wantCreated {
				t.Fatalf("sent=%v created=%d", sent, api.created.Load())
			}
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
			} else {
				if err != nil || resp.StatusCode != c.wantStatus {
					t.Fatalf("resp=%v err=%v", resp, err)
				}
				_ = resp.Body.Close()
			}
			if j := tr.Journal(); len(j) != 1 || j[0].Fault == "" {
				t.Fatalf("journal %+v", j)
			}
			// The fault was used up: the next request goes through.
			resp, _, err = httpsent.Do(tr.Client(), postReq(t, context.Background(), "https://"+host+"/posts"))
			if err != nil || resp.StatusCode != http.StatusCreated {
				t.Fatalf("after the fault: %v %v", resp, err)
			}
			_ = resp.Body.Close()
		})
	}
}

func TestPanickingHandlerFailsAfterSending(t *testing.T) {
	tr := stubnet.New()
	tr.Handle(host, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	_, sent, err := httpsent.Do(tr.Client(), postReq(t, context.Background(), "https://"+host+"/posts"))
	if err == nil || !sent || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
}

func TestCancelledBeforeSendingIsNotDelivered(t *testing.T) {
	tr, api := newNet(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, sent, err := httpsent.Do(tr.Client(), postReq(t, ctx, "https://"+host+"/posts"))
	if err == nil || sent || api.created.Load() != 0 {
		t.Fatalf("sent=%v err=%v created=%d", sent, err, api.created.Load())
	}
}

func TestJournalIsBounded(t *testing.T) {
	tr, _ := newNet(t)
	tr.MaxEntries = 3
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("https://%s/item/%d", host, i), nil)
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	j := tr.Journal()
	if len(j) != 3 || j[0].Path != "/item/2" || j[2].Path != "/item/4" {
		t.Fatalf("journal %+v", j)
	}
}

func TestConcurrentUse(t *testing.T) {
	tr, api := newNet(t)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _, err := httpsent.Do(tr.Client(), postReq(t, context.Background(), "https://"+host+"/posts"))
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	if api.created.Load() != 32 || len(tr.Journal()) != 32 {
		t.Fatalf("created=%d journal=%d", api.created.Load(), len(tr.Journal()))
	}
}

// ---------- with sendguard: at most once over a stub network ----------

// sendPost is what an adapter would do: send, classify the outcome for
// sendguard (not sent / definite status → Failed, anything else → Unknown).
func sendPost(c *http.Client) sendguard.SendFunc {
	return func(ctx context.Context) (sendguard.Result, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/posts", strings.NewReader(`{"text":"hello"}`))
		if err != nil {
			return sendguard.Result{}, sendguard.Reject(err)
		}
		resp, sent, err := httpsent.Do(c, req)
		if err != nil {
			return sendguard.Result{}, httpsent.Classify(err, sent)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return sendguard.Result{}, &httpsent.StatusError{Code: resp.StatusCode}
		}
		var out struct{ ID string }
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return sendguard.Result{}, err // created, but we cannot tell which post: Unknown
		}
		return sendguard.Result{ExternalRef: out.ID}, nil
	}
}

func TestAtMostOnceWithSendguard(t *testing.T) {
	cases := []struct {
		name        string
		fault       *stubnet.Fault
		wantState   sendguard.State
		wantErr     error
		wantCreated int64
	}{
		{"delivered", nil, sendguard.Succeeded, nil, 1},
		{"refused before sending → Failed (nothing happened)", &stubnet.Refuse, sendguard.Failed, sendguard.ErrRejected, 0},
		{"response lost → Unknown (the post exists)", &stubnet.Lose, sendguard.Unknown, sendguard.ErrUnknown, 1},
		{"400 → Failed", ptr(stubnet.Status(http.StatusBadRequest)), sendguard.Failed, sendguard.ErrRejected, 0},
		{"503 → Unknown", ptr(stubnet.Status(http.StatusServiceUnavailable)), sendguard.Unknown, sendguard.ErrUnknown, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, api := newNet(t)
			if c.fault != nil {
				tr.Inject(host, *c.fault)
			}
			ctx := context.Background()
			g := sendguard.New(sendguard.NewMemStore())
			if _, err := g.Enqueue(ctx, "post-1"); err != nil {
				t.Fatal(err)
			}
			rec, err := g.Run(ctx, "post-1", sendPost(tr.Client()))
			if rec.State != c.wantState || (c.wantErr == nil) != (err == nil) || (c.wantErr != nil && !errors.Is(err, c.wantErr)) {
				t.Fatalf("Run = %s, %v", rec.State, err)
			}
			if c.wantState == sendguard.Succeeded && rec.Result.ExternalRef != "post-1" {
				t.Fatalf("external ref %q", rec.Result.ExternalRef)
			}
			// Whatever happened, a second Run never sends again.
			if _, err := g.Run(ctx, "post-1", sendPost(tr.Client())); !errors.Is(err, sendguard.ErrNotReady) {
				t.Fatalf("second Run: %v", err)
			}
			if api.created.Load() != c.wantCreated {
				t.Fatalf("created %d posts, want %d", api.created.Load(), c.wantCreated)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
