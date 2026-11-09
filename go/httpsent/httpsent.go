// Package httpsent tells whether an HTTP request may have reached the server,
// so that a failed side effect can be classified as "definitely not done"
// (safe to try again) or "may have been done" (must be reconciled first).
//
// A request whose headers were never written to a connection (DNS, dial or
// TLS failure, a refused connection, a context cancelled before writing)
// cannot have been acted on by the server. Any failure after that point —
// a timeout while waiting for the response, a connection reset, a response
// that cannot be read — is ambiguous: the server may have received the
// request and acted on it.
//
// [Do] sends a request and reports which case applies; [Classify] turns the
// result into an error that [github.com/caprico99278/FledgeFlow-oss/go/sendguard]
// understands: a failure before sending is a [*NotSentError], whose Definite
// method reports true, and anything else is returned unchanged (ambiguous).
// [DefiniteStatus] does the same for HTTP error statuses.
package httpsent

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
)

// Track returns req with a client trace that records when its headers are
// written, and a function that reports whether they were. Use it when the
// request is sent by code you do not control; otherwise use [Do].
//
// Only the WroteHeaders hook is added; hooks already present in the
// request's context keep running.
func Track(req *http.Request) (*http.Request, func() bool) {
	var wrote atomic.Bool // the hook may run on a transport goroutine
	trace := &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), wrote.Load
}

// Do sends req with c (http.DefaultClient when nil) and reports whether any
// part of the request may have reached the server. sent is false only when
// the headers were never written: the server cannot have acted on it. When
// err is nil, sent is true and the caller must close resp.Body.
func Do(c *http.Client, req *http.Request) (resp *http.Response, sent bool, err error) {
	if c == nil {
		c = http.DefaultClient
	}
	req, wrote := Track(req)
	resp, err = c.Do(req)
	if err != nil {
		return nil, wrote(), err
	}
	return resp, true, nil
}

// NotSentError is a failure that happened before the request was written:
// the server never saw it. Its Definite method reports true, so
// sendguard records the side effect as Failed (not Unknown).
type NotSentError struct{ Err error }

func (e *NotSentError) Error() string {
	return fmt.Sprintf("request not sent: %v", e.Err)
}

func (e *NotSentError) Unwrap() error { return e.Err }

// Definite reports that the outcome is certain: nothing was done.
func (e *NotSentError) Definite() bool { return true }

// Classify maps the error from [Do] (or a [Track]ed send): nil stays nil, a
// failure before sending becomes a [*NotSentError], and a failure after
// sending is returned unchanged — it is ambiguous and must not be retried
// without reconciliation.
func Classify(err error, sent bool) error {
	if err == nil {
		return nil
	}
	if !sent {
		return &NotSentError{Err: err}
	}
	return err
}

// NotSent reports whether err (anywhere in its chain) is a [*NotSentError].
func NotSent(err error) bool {
	var e *NotSentError
	return errors.As(err, &e)
}

// DefiniteStatus reports whether an HTTP response status means the server
// refused the request without acting on it: every 4xx except 408 (Request
// Timeout, which says nothing about whether the request was processed).
// 2xx and 3xx are not failures; 5xx are ambiguous (the server may have acted
// before failing).
//
// 429 (Too Many Requests) counts as definite: servers reject before doing the
// work. If a service documents otherwise for some endpoint, classify that
// status yourself.
func DefiniteStatus(code int) bool {
	return code >= 400 && code < 500 && code != http.StatusRequestTimeout
}

// StatusError is an HTTP error response. Definite reports [DefiniteStatus],
// so sendguard records 4xx (except 408) as Failed and other statuses as
// Unknown.
type StatusError struct {
	Code int
	// Detail is a short human-readable excerpt (never parsed).
	Detail string
}

func (e *StatusError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("HTTP %d %s", e.Code, http.StatusText(e.Code))
	}
	return fmt.Sprintf("HTTP %d %s: %s", e.Code, http.StatusText(e.Code), e.Detail)
}

// Definite reports whether the status is a definite refusal ([DefiniteStatus]).
func (e *StatusError) Definite() bool { return DefiniteStatus(e.Code) }
