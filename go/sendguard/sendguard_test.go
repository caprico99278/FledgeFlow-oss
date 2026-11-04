package sendguard_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caprico99278/FledgeFlow-oss/go/sendguard"
)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock { return &fakeClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func setup(t *testing.T, id string) (*sendguard.Guard, *sendguard.MemStore, *fakeClock) {
	t.Helper()
	store := sendguard.NewMemStore()
	clock := newClock()
	g := sendguard.New(store)
	g.Now = clock.Now
	g.Owner = "worker-test"
	if _, err := g.Enqueue(context.Background(), id); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return g, store, clock
}

func mustState(t *testing.T, s sendguard.Store, id string, want sendguard.State) sendguard.Record {
	t.Helper()
	rec, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	if rec.State != want {
		t.Fatalf("state of %q = %q, want %q", id, rec.State, want)
	}
	return rec
}

func TestRunSuccess(t *testing.T) {
	g, store, _ := setup(t, "job-1")
	ctx := context.Background()
	rec, err := g.Run(ctx, "job-1", func(ctx context.Context) (sendguard.Result, error) {
		return sendguard.Result{ExternalRef: "ext-42"}, nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.State != sendguard.Succeeded || rec.Result.ExternalRef != "ext-42" || rec.Attempts != 1 {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Owner != "worker-test" {
		t.Errorf("owner = %q", rec.Owner)
	}
	mustState(t, store, "job-1", sendguard.Succeeded)

	// A finished record is never sent again.
	_, err = g.Run(ctx, "job-1", func(context.Context) (sendguard.Result, error) {
		t.Fatal("send called on a Succeeded record")
		return sendguard.Result{}, nil
	})
	if !errors.Is(err, sendguard.ErrNotReady) {
		t.Fatalf("second Run err = %v, want ErrNotReady", err)
	}
}

func TestSubmittingIsPersistedBeforeSend(t *testing.T) {
	g, store, _ := setup(t, "job-1")
	_, err := g.Run(context.Background(), "job-1", func(ctx context.Context) (sendguard.Result, error) {
		rec := mustState(t, store, "job-1", sendguard.Submitting)
		if rec.Attempts != 1 {
			t.Errorf("attempts during send = %d", rec.Attempts)
		}
		return sendguard.Result{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentRunSendsExactlyOnce(t *testing.T) {
	g, store, _ := setup(t, "job-1")
	const workers = 100
	var calls, wins, notReady atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := g.Run(context.Background(), "job-1", func(context.Context) (sendguard.Result, error) {
				calls.Add(1)
				time.Sleep(5 * time.Millisecond) // widen the race window
				return sendguard.Result{ExternalRef: "once"}, nil
			})
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, sendguard.ErrNotReady):
				notReady.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("send called %d times, want exactly 1", calls.Load())
	}
	if wins.Load() != 1 || notReady.Load() != workers-1 {
		t.Fatalf("wins=%d notReady=%d", wins.Load(), notReady.Load())
	}
	mustState(t, store, "job-1", sendguard.Succeeded)
}

func TestConcurrentRunManyIDs(t *testing.T) {
	store := sendguard.NewMemStore()
	g := sendguard.New(store)
	ctx := context.Background()
	const ids, workersPerID = 20, 10
	for i := range ids {
		if _, err := g.Enqueue(ctx, fmt.Sprintf("id-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	var calls [ids]atomic.Int64
	var wg sync.WaitGroup
	for i := range ids {
		for range workersPerID {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = g.Run(ctx, fmt.Sprintf("id-%d", i), func(context.Context) (sendguard.Result, error) {
					calls[i].Add(1)
					return sendguard.Result{}, nil
				})
			}()
		}
	}
	wg.Wait()
	for i := range ids {
		if n := calls[i].Load(); n != 1 {
			t.Errorf("id-%d sent %d times", i, n)
		}
	}
}

func TestDefiniteRejectionIsFailed(t *testing.T) {
	g, store, _ := setup(t, "job-1")
	cause := errors.New("400 bad request")
	rec, err := g.Run(context.Background(), "job-1", func(context.Context) (sendguard.Result, error) {
		return sendguard.Result{}, sendguard.Reject(cause)
	})
	if !errors.Is(err, sendguard.ErrRejected) || !errors.Is(err, cause) {
		t.Fatalf("err = %v, want ErrRejected wrapping cause", err)
	}
	if rec.State != sendguard.Failed || rec.LastError != "400 bad request" {
		t.Fatalf("record = %+v", rec)
	}
	mustState(t, store, "job-1", sendguard.Failed)
}

// definiteFalse implements Definite but reports false: must be Unknown.
type definiteFalse struct{}

func (definiteFalse) Error() string  { return "maybe" }
func (definiteFalse) Definite() bool { return false }

func TestAmbiguousOutcomesAreUnknown(t *testing.T) {
	cases := map[string]sendguard.SendFunc{
		"plain error": func(context.Context) (sendguard.Result, error) {
			return sendguard.Result{}, errors.New("connection reset")
		},
		"definite false": func(context.Context) (sendguard.Result, error) {
			return sendguard.Result{}, definiteFalse{}
		},
		"deadline": func(ctx context.Context) (sendguard.Result, error) {
			ctx, cancel := context.WithTimeout(ctx, time.Millisecond)
			defer cancel()
			<-ctx.Done()
			return sendguard.Result{}, ctx.Err()
		},
		"panic": func(context.Context) (sendguard.Result, error) {
			panic("boom")
		},
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			g, store, _ := setup(t, "job-1")
			rec, err := g.Run(context.Background(), "job-1", send)
			if !errors.Is(err, sendguard.ErrUnknown) {
				t.Fatalf("err = %v, want ErrUnknown", err)
			}
			if rec.State != sendguard.Unknown || rec.LastError == "" {
				t.Fatalf("record = %+v", rec)
			}
			mustState(t, store, "job-1", sendguard.Unknown)
			if name == "panic" {
				var pe *sendguard.PanicError
				if !errors.As(err, &pe) || pe.Value != "boom" {
					t.Fatalf("err = %v, want PanicError(boom)", err)
				}
			}
		})
	}
}

func TestCancelledContextStillPersistsUnknown(t *testing.T) {
	g, store, _ := setup(t, "job-1")
	ctx, cancel := context.WithCancel(context.Background())
	rec, err := g.Run(ctx, "job-1", func(ctx context.Context) (sendguard.Result, error) {
		cancel() // caller gives up while the request is in flight
		return sendguard.Result{}, ctx.Err()
	})
	if !errors.Is(err, sendguard.ErrUnknown) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if rec.State != sendguard.Unknown {
		t.Fatalf("state = %q", rec.State)
	}
	mustState(t, store, "job-1", sendguard.Unknown)
}

func TestUnknownIsNeverRetried(t *testing.T) {
	g, store, _ := setup(t, "job-1")
	ctx := context.Background()
	var calls atomic.Int64
	send := func(context.Context) (sendguard.Result, error) {
		calls.Add(1)
		return sendguard.Result{}, errors.New("timeout")
	}
	if _, err := g.Run(ctx, "job-1", send); !errors.Is(err, sendguard.ErrUnknown) {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := g.Run(ctx, "job-1", send); !errors.Is(err, sendguard.ErrNotReady) {
			t.Fatalf("retry err = %v, want ErrNotReady", err)
		}
	}
	// Recover and Cancel do not move it either.
	if _, err := g.Recover(ctx, "job-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Cancel(ctx, "job-1"); !errors.Is(err, sendguard.ErrNotReady) {
		t.Fatalf("Cancel err = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("send called %d times, want 1", calls.Load())
	}
	mustState(t, store, "job-1", sendguard.Unknown)
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	unknown := func(context.Context) (sendguard.Result, error) {
		return sendguard.Result{}, errors.New("timeout")
	}

	t.Run("succeeded", func(t *testing.T) {
		g, _, _ := setup(t, "job-1")
		_, _ = g.Run(ctx, "job-1", unknown)
		rec, err := g.Resolve(ctx, "job-1", sendguard.Outcome{
			State: sendguard.Succeeded, Result: sendguard.Result{ExternalRef: "found-it"},
		})
		if err != nil || rec.State != sendguard.Succeeded || rec.Result.ExternalRef != "found-it" {
			t.Fatalf("rec=%+v err=%v", rec, err)
		}
	})

	t.Run("ready re-arms", func(t *testing.T) {
		g, _, _ := setup(t, "job-1")
		_, _ = g.Run(ctx, "job-1", unknown)
		if _, err := g.Resolve(ctx, "job-1", sendguard.Outcome{State: sendguard.Ready, Note: "verified absent"}); err != nil {
			t.Fatal(err)
		}
		rec, err := g.Run(ctx, "job-1", func(context.Context) (sendguard.Result, error) {
			return sendguard.Result{}, nil
		})
		if err != nil || rec.State != sendguard.Succeeded || rec.Attempts != 2 {
			t.Fatalf("rec=%+v err=%v", rec, err)
		}
	})

	t.Run("only from unknown", func(t *testing.T) {
		g, _, _ := setup(t, "job-1")
		_, err := g.Resolve(ctx, "job-1", sendguard.Outcome{State: sendguard.Succeeded})
		if !errors.Is(err, sendguard.ErrNotReady) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("invalid outcome", func(t *testing.T) {
		g, _, _ := setup(t, "job-1")
		_, _ = g.Run(ctx, "job-1", unknown)
		if _, err := g.Resolve(ctx, "job-1", sendguard.Outcome{State: sendguard.Submitting}); err == nil {
			t.Fatal("expected error")
		}
	})
}

// TestCrashAfterSubmitting simulates a worker process that dies after
// persisting Submitting but before recording the outcome.
func TestCrashAfterSubmitting(t *testing.T) {
	g, store, clock := setup(t, "job-1")
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = g.Run(ctx, "job-1", func(context.Context) (sendguard.Result, error) {
			// The request leaves the process, then the process "dies":
			// Goexit stops this goroutine without returning to Run.
			runtime.Goexit()
			return sendguard.Result{}, nil
		})
		t.Error("Run returned after Goexit")
	}()
	<-done
	mustState(t, store, "job-1", sendguard.Submitting)

	// A restarted worker (fresh Guard, same store) must not send again.
	restarted := sendguard.New(store)
	restarted.Now = clock.Now
	var calls atomic.Int64
	send := func(context.Context) (sendguard.Result, error) {
		calls.Add(1)
		return sendguard.Result{}, nil
	}
	if _, err := restarted.Run(ctx, "job-1", send); !errors.Is(err, sendguard.ErrNotReady) {
		t.Fatalf("Run after crash err = %v", err)
	}

	// Not stale yet: Recover leaves it alone.
	if rec, err := restarted.Recover(ctx, "job-1", time.Minute); err != nil || rec.State != sendguard.Submitting {
		t.Fatalf("early Recover rec=%+v err=%v", rec, err)
	}
	clock.Advance(2 * time.Minute)
	rec, err := restarted.Recover(ctx, "job-1", time.Minute)
	if err != nil || rec.State != sendguard.Unknown {
		t.Fatalf("Recover rec=%+v err=%v", rec, err)
	}
	if _, err := restarted.Run(ctx, "job-1", send); !errors.Is(err, sendguard.ErrNotReady) {
		t.Fatalf("Run after recover err = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("send called %d times after crash", calls.Load())
	}
	if _, err := restarted.Resolve(ctx, "job-1", sendguard.Outcome{State: sendguard.Succeeded}); err != nil {
		t.Fatal(err)
	}
}

func TestStaleClaimReturnsToReady(t *testing.T) {
	g, store, clock := setup(t, "job-1")
	ctx := context.Background()
	// A worker claimed the record and died before persisting Submitting.
	rec := mustState(t, store, "job-1", sendguard.Ready)
	if _, ok, err := store.CompareAndSwap(ctx, "job-1", sendguard.Ready, rec.Version, sendguard.Claimed, rec); !ok || err != nil {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	clock.Advance(time.Hour)
	if rec, err := g.Recover(ctx, "job-1", time.Minute); err != nil || rec.State != sendguard.Ready {
		t.Fatalf("Recover rec=%+v err=%v", rec, err)
	}
	var calls atomic.Int64
	_, err := g.Run(ctx, "job-1", func(context.Context) (sendguard.Result, error) {
		calls.Add(1)
		return sendguard.Result{}, nil
	})
	if err != nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestSlowWorkerLosesToRecover(t *testing.T) {
	g, store, clock := setup(t, "job-1")
	ctx := context.Background()
	rec, err := g.Run(ctx, "job-1", func(context.Context) (sendguard.Result, error) {
		// While the send is in flight, an over-eager recovery marks it Unknown.
		clock.Advance(time.Hour)
		other := sendguard.New(store)
		other.Now = clock.Now
		if _, err := other.Recover(ctx, "job-1", time.Minute); err != nil {
			t.Error(err)
		}
		return sendguard.Result{ExternalRef: "late"}, nil
	})
	if !errors.Is(err, sendguard.ErrLostUpdate) {
		t.Fatalf("err = %v, want ErrLostUpdate", err)
	}
	if rec.State != sendguard.Unknown {
		t.Fatalf("state = %q, want unknown (needs reconciliation)", rec.State)
	}
}

func TestCancel(t *testing.T) {
	g, store, _ := setup(t, "job-1")
	ctx := context.Background()
	if _, err := g.Cancel(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}
	mustState(t, store, "job-1", sendguard.Cancelled)
	_, err := g.Run(ctx, "job-1", func(context.Context) (sendguard.Result, error) {
		t.Fatal("send on cancelled record")
		return sendguard.Result{}, nil
	})
	if !errors.Is(err, sendguard.ErrNotReady) {
		t.Fatalf("err = %v", err)
	}
}

func TestEnqueueAndNotFound(t *testing.T) {
	g, _, _ := setup(t, "job-1")
	ctx := context.Background()
	if _, err := g.Enqueue(ctx, "job-1"); !errors.Is(err, sendguard.ErrNotReady) {
		t.Fatalf("duplicate Enqueue err = %v", err)
	}
	if _, err := g.Run(ctx, "missing", nil); !errors.Is(err, sendguard.ErrNotFound) {
		t.Fatalf("Run(missing) err = %v", err)
	}
}

func TestMemStoreContract(t *testing.T) {
	ctx := context.Background()
	s := sendguard.NewMemStore()
	if _, err := s.Get(ctx, "x"); !errors.Is(err, sendguard.ErrNotFound) {
		t.Fatalf("Get missing err = %v", err)
	}
	// Create from (None, 0).
	rec, ok, err := s.CompareAndSwap(ctx, "x", sendguard.None, 0, sendguard.Ready, sendguard.Record{ID: "ignored", Version: 99})
	if err != nil || !ok || rec.ID != "x" || rec.Version != 1 || rec.State != sendguard.Ready {
		t.Fatalf("create rec=%+v ok=%v err=%v", rec, ok, err)
	}
	// Stale version: no swap, no error, current record returned.
	rec, ok, err = s.CompareAndSwap(ctx, "x", sendguard.Ready, 0, sendguard.Claimed, rec)
	if err != nil || ok || rec.Version != 1 {
		t.Fatalf("stale rec=%+v ok=%v err=%v", rec, ok, err)
	}
	// Wrong state.
	if _, ok, _ = s.CompareAndSwap(ctx, "x", sendguard.Claimed, 1, sendguard.Submitting, rec); ok {
		t.Fatal("swap with wrong from-state succeeded")
	}
	// Disallowed transition is an error.
	if _, ok, err = s.CompareAndSwap(ctx, "x", sendguard.Ready, 1, sendguard.Succeeded, rec); ok || err == nil {
		t.Fatalf("Ready->Succeeded ok=%v err=%v", ok, err)
	}
	// Missing record with non-None from.
	if rec, ok, err = s.CompareAndSwap(ctx, "y", sendguard.Ready, 1, sendguard.Claimed, sendguard.Record{}); ok || err != nil || rec.ID != "" {
		t.Fatalf("missing rec=%+v ok=%v err=%v", rec, ok, err)
	}
	// Cancelled context.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err = s.CompareAndSwap(cctx, "x", sendguard.Ready, 1, sendguard.Claimed, rec); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx err = %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d", s.Len())
	}
}

func TestTransitionTable(t *testing.T) {
	for _, s := range []sendguard.State{sendguard.Succeeded, sendguard.Failed, sendguard.Cancelled} {
		if !sendguard.Transitions.Terminal(s) {
			t.Errorf("%q should be terminal", s)
		}
	}
	if sendguard.Transitions.Can(sendguard.Unknown, sendguard.Submitting) {
		t.Error("Unknown must never go back to Submitting directly")
	}
	if sendguard.Transitions.Can(sendguard.Submitting, sendguard.Ready) {
		t.Error("Submitting must never go back to Ready")
	}
}

func ExampleGuard_Run() {
	ctx := context.Background()
	g := sendguard.New(sendguard.NewMemStore())
	_, _ = g.Enqueue(ctx, "notify-42")

	rec, err := g.Run(ctx, "notify-42", func(ctx context.Context) (sendguard.Result, error) {
		// Call the external system here, passing ctx along.
		return sendguard.Result{ExternalRef: "remote-1"}, nil
	})
	fmt.Println(rec.State, rec.Result.ExternalRef, err)

	// A second Run never sends again.
	_, err = g.Run(ctx, "notify-42", func(context.Context) (sendguard.Result, error) {
		panic("not reached")
	})
	fmt.Println(errors.Is(err, sendguard.ErrNotReady))
	// Output:
	// succeeded remote-1 <nil>
	// true
}

func ExampleGuard_Resolve() {
	ctx := context.Background()
	g := sendguard.New(sendguard.NewMemStore())
	_, _ = g.Enqueue(ctx, "job")

	// The request timed out: did the remote side get it? Nobody knows.
	rec, _ := g.Run(ctx, "job", func(context.Context) (sendguard.Result, error) {
		return sendguard.Result{}, context.DeadlineExceeded
	})
	fmt.Println(rec.State)

	// A reconciliation step looked it up on the remote side and found it.
	rec, _ = g.Resolve(ctx, "job", sendguard.Outcome{
		State:  sendguard.Succeeded,
		Result: sendguard.Result{ExternalRef: "remote-7"},
	})
	fmt.Println(rec.State, rec.Result.ExternalRef)
	// Output:
	// unknown
	// succeeded remote-7
}

func ExampleReject() {
	ctx := context.Background()
	g := sendguard.New(sendguard.NewMemStore())
	_, _ = g.Enqueue(ctx, "job")
	rec, err := g.Run(ctx, "job", func(context.Context) (sendguard.Result, error) {
		// The remote side answered "invalid payload": it definitely did
		// not take effect, so the record can be Failed instead of Unknown.
		return sendguard.Result{}, sendguard.Reject(errors.New("invalid payload"))
	})
	fmt.Println(rec.State, errors.Is(err, sendguard.ErrRejected))
	// Output: failed true
}
