package httpsent_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/caprico99278/FledgeFlow-oss/go/httpsent"
)

type definite interface{ Definite() bool }

func isDefinite(err error) bool {
	var d definite
	return errors.As(err, &d) && d.Definite()
}

func post(t *testing.T, ctx context.Context, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestSuccessIsSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	resp, sent, err := httpsent.Do(srv.Client(), post(t, context.Background(), srv.URL))
	if err != nil || !sent || resp.StatusCode != http.StatusCreated {
		t.Fatalf("Do = %v, sent=%v, err=%v", resp, sent, err)
	}
	_ = resp.Body.Close()
}

// A refused connection never reaches a server: definitely not done.
func TestRefusedConnectionIsNotSent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // nothing listens there any more
	_, sent, err := httpsent.Do(nil, post(t, context.Background(), "http://"+addr+"/send"))
	if err == nil || sent {
		t.Fatalf("sent=%v err=%v, want a failure before sending", sent, err)
	}
	cerr := httpsent.Classify(err, sent)
	if !httpsent.NotSent(cerr) || !isDefinite(cerr) || !errors.Is(cerr, err) {
		t.Fatalf("Classify = %v (definite=%v)", cerr, isDefinite(cerr))
	}
}

// The server read the request and dropped the connection without answering:
// it may have acted, so the failure is ambiguous.
func TestFailureAfterWritingIsAmbiguous(t *testing.T) {
	received := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received <- struct{}{}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()
	_, sent, err := httpsent.Do(srv.Client(), post(t, context.Background(), srv.URL))
	<-received
	if err == nil || !sent {
		t.Fatalf("sent=%v err=%v, want an error after sending", sent, err)
	}
	cerr := httpsent.Classify(err, sent)
	if httpsent.NotSent(cerr) || isDefinite(cerr) || cerr != err {
		t.Fatalf("Classify = %v, want the original (ambiguous) error", cerr)
	}
}

// A timeout while waiting for the answer is ambiguous too.
func TestTimeoutAfterWritingIsAmbiguous(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, sent, err := httpsent.Do(srv.Client(), post(t, ctx, srv.URL))
	if err == nil || !sent || isDefinite(httpsent.Classify(err, sent)) {
		t.Fatalf("sent=%v err=%v, want an ambiguous timeout", sent, err)
	}
}

func TestCancelledBeforeSendingIsNotSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the server must not see a request whose context was already cancelled")
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, sent, err := httpsent.Do(srv.Client(), post(t, ctx, srv.URL))
	if err == nil || sent || !isDefinite(httpsent.Classify(err, sent)) {
		t.Fatalf("sent=%v err=%v", sent, err)
	}
}

// Hooks the caller put in the context keep running next to Track's own.
func TestTrackKeepsExistingHooks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	gotConn := false
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { gotConn = true }})
	req, wrote := httpsent.Track(post(t, ctx, srv.URL))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !wrote() || !gotConn {
		t.Fatalf("wrote=%v gotConn=%v", wrote(), gotConn)
	}
}

func TestClassifyNil(t *testing.T) {
	if httpsent.Classify(nil, false) != nil || httpsent.Classify(nil, true) != nil {
		t.Fatal("Classify(nil) must be nil")
	}
}

func TestDefiniteStatus(t *testing.T) {
	for code, want := range map[int]bool{
		200: false, 201: false, 302: false,
		400: true, 401: true, 403: true, 404: true, 409: true, 422: true, 429: true,
		408: false, // says nothing about whether the request was processed
		500: false, 502: false, 503: false, 504: false,
	} {
		if got := httpsent.DefiniteStatus(code); got != want {
			t.Errorf("DefiniteStatus(%d) = %v, want %v", code, got, want)
		}
		if code >= 400 {
			e := &httpsent.StatusError{Code: code, Detail: "x"}
			if isDefinite(e) != want || !strings.Contains(e.Error(), "HTTP ") {
				t.Errorf("StatusError{%d}: definite=%v msg=%q", code, isDefinite(e), e.Error())
			}
		}
	}
}
