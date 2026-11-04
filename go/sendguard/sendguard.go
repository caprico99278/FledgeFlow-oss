// Package sendguard implements an at-most-once protocol for side effects
// sent to an external system that may or may not have received them
// (an HTTP POST that timed out, a message whose acknowledgement was lost).
//
// # Protocol
//
// Every side effect is a [Record] identified by a caller-chosen id and kept
// in a [Store] that supports compare-and-swap on (state, version):
//
//		None       --Enqueue--> Ready
//		Ready      --Run------> Claimed --> Submitting --send--> Succeeded | Failed | Unknown
//		Ready, Claimed      --Cancel---> Cancelled
//		Claimed    (stale)  --Recover--> Ready
//		Submitting (stale)  --Recover--> Unknown
//		Unknown             --Resolve--> Succeeded | Failed | Ready
//
//	 1. [Guard.Run] claims the record with a CAS Ready → Claimed. Exactly one
//	    concurrent caller wins; the others get [ErrNotReady].
//	 2. It persists Submitting *before* invoking the send function. From this
//	    point on, the side effect may have happened.
//	 3. The result is mapped to a final state:
//	    success → Succeeded; an error with Definite() == true (see [Definite])
//	    → Failed; anything else — timeouts, context cancellation, unknown
//	    errors, panics — → Unknown.
//
// Unknown is never retried automatically: someone (an operator or a
// reconciliation job that can query the external system) must call
// [Guard.Resolve]. Resolving to Ready re-arms the record for another Run,
// which is only correct when the external system is known not to have
// received the first attempt.
//
// If a process dies, [Guard.Recover] cleans up stale records: a stale
// Claimed record is returned to Ready (nothing was sent yet) and a stale
// Submitting record becomes Unknown (it may have been sent).
//
// The protocol guarantees *at most once*, not *exactly once*: when in doubt,
// it stops and asks for reconciliation instead of sending twice.
package sendguard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/caprico99278/FledgeFlow-oss/go/fsm"
)

// State is the lifecycle state of a [Record].
type State string

// Lifecycle states.
const (
	// None is the state of a record that does not exist yet (version 0).
	None       State = ""
	Ready      State = "ready"
	Claimed    State = "claimed"
	Submitting State = "submitting"
	Succeeded  State = "succeeded"
	Failed     State = "failed"
	Unknown    State = "unknown"
	Cancelled  State = "cancelled"
)

// Transitions is the complete transition table of the protocol. Stores may
// use it to reject invalid writes (the reference [MemStore] does).
var Transitions = fsm.Table[State]{
	None:       {Ready},
	Ready:      {Claimed, Cancelled},
	Claimed:    {Submitting, Ready, Cancelled},
	Submitting: {Succeeded, Failed, Unknown},
	Unknown:    {Succeeded, Failed, Ready},
}

// Result is what a successful (or reconciled) send reports.
type Result struct {
	// ExternalRef is an identifier assigned by the external system, if any.
	ExternalRef string
	// Note is free-form detail for humans.
	Note string
}

// Record is the persisted state of one side effect.
type Record struct {
	ID      string
	State   State
	Version int64 // incremented by every successful CompareAndSwap
	// Owner identifies the Guard that claimed the record (diagnostics only).
	Owner string
	// Attempts counts how many times the send function was invoked.
	Attempts int
	// UpdatedAt is set by the Guard on every transition.
	UpdatedAt time.Time
	// Result is set when the record reaches Succeeded.
	Result Result
	// LastError is the message of the error that led to Failed or Unknown.
	LastError string
}

// Store persists records with optimistic concurrency.
//
// Implementations must make CompareAndSwap atomic and linearizable for a
// given id: concurrent calls with the same (from, fromVersion) must have at
// most one winner.
type Store interface {
	// Get returns the record or an error wrapping [ErrNotFound].
	Get(ctx context.Context, id string) (Record, error)
	// CompareAndSwap replaces the record with patch if, and only if, its
	// current state is from and its version is fromVersion. A missing
	// record behaves as state [None] at version 0, so a CAS from (None, 0)
	// creates it. On success the stored record is patch with ID = id,
	// State = to and Version = fromVersion+1, and it is returned with
	// swapped = true. On a mismatch the current record (zero Record if
	// missing) is returned with swapped = false and a nil error.
	CompareAndSwap(ctx context.Context, id string, from State, fromVersion int64, to State, patch Record) (rec Record, swapped bool, err error)
}

// Sentinel errors. Use [errors.Is] to test for them.
var (
	// ErrNotFound: no record with this id.
	ErrNotFound = errors.New("sendguard: record not found")
	// ErrNotReady: the record is not in the state the operation needs
	// (already claimed by someone else, finished, Unknown, ...). The send
	// function was not invoked.
	ErrNotReady = errors.New("sendguard: record not in required state")
	// ErrRejected: the send function reported a definite rejection;
	// the record is Failed.
	ErrRejected = errors.New("sendguard: send definitely rejected")
	// ErrUnknown: the outcome of the send is ambiguous; the record is
	// Unknown and needs Resolve.
	ErrUnknown = errors.New("sendguard: send outcome unknown")
	// ErrLostUpdate: the record changed underneath a running send (for
	// example Recover marked it Unknown); the final outcome was not
	// persisted. The record still needs reconciliation.
	ErrLostUpdate = errors.New("sendguard: record changed during send")
)

// Definite is implemented by errors that are known to be final rejections:
// the external system received the request and refused it, so it did not
// and will not take effect (validation error, permission denied, ...).
type Definite interface {
	Definite() bool
}

// Reject wraps err as a definite rejection.
func Reject(err error) error { return &definiteError{err: err} }

type definiteError struct{ err error }

func (e *definiteError) Error() string  { return e.err.Error() }
func (e *definiteError) Unwrap() error  { return e.err }
func (e *definiteError) Definite() bool { return true }

// PanicError carries a panic recovered from the send function.
type PanicError struct{ Value any }

func (e *PanicError) Error() string { return fmt.Sprintf("send panicked: %v", e.Value) }

// SendFunc performs the side effect. It must honour ctx.
type SendFunc func(ctx context.Context) (Result, error)

// Guard runs side effects under the protocol. The zero value is not usable;
// create one with [New].
type Guard struct {
	store Store
	// Now returns the current time; replace it in tests.
	Now func() time.Time
	// Owner is recorded in claimed records for diagnostics.
	Owner string
}

// New returns a Guard backed by store.
func New(store Store) *Guard {
	return &Guard{store: store, Now: time.Now}
}

func (g *Guard) now() time.Time {
	if g.Now == nil {
		return time.Now()
	}
	return g.Now()
}

func notReady(rec Record) error {
	return fmt.Errorf("%w: %q is %q", ErrNotReady, rec.ID, rec.State)
}

// Enqueue creates a Ready record. It returns [ErrNotReady] if the id
// already exists (in any state), which makes Enqueue idempotent-safe.
func (g *Guard) Enqueue(ctx context.Context, id string) (Record, error) {
	rec, ok, err := g.store.CompareAndSwap(ctx, id, None, 0, Ready, Record{UpdatedAt: g.now()})
	if err != nil {
		return rec, err
	}
	if !ok {
		return rec, notReady(rec)
	}
	return rec, nil
}

// Run claims the record, persists Submitting, invokes send at most once and
// persists the outcome. It returns the final record and:
//
//   - nil when the record is Succeeded;
//   - an error wrapping [ErrRejected] and the send error when Failed;
//   - an error wrapping [ErrUnknown] and the cause when Unknown (including
//     a *[PanicError] when send panicked; the panic is not re-raised);
//   - an error wrapping [ErrNotReady] when send was not invoked because the
//     record was not Ready or another caller won the claim;
//   - a store error, possibly wrapping [ErrLostUpdate].
//
// The outcome is persisted with a context detached from ctx's cancellation,
// so a cancelled ctx still records Unknown rather than leaving Submitting.
func (g *Guard) Run(ctx context.Context, id string, send SendFunc) (Record, error) {
	cur, err := g.store.Get(ctx, id)
	if err != nil {
		return cur, err
	}
	if cur.State != Ready {
		return cur, notReady(cur)
	}

	// 1. Claim: only one concurrent caller can win this CAS.
	claim := cur
	claim.Owner = g.Owner
	claim.UpdatedAt = g.now()
	cur, ok, err := g.store.CompareAndSwap(ctx, id, Ready, cur.Version, Claimed, claim)
	if err != nil {
		return cur, err
	}
	if !ok {
		return cur, notReady(cur)
	}

	// 2. Persist Submitting BEFORE the side effect.
	sub := cur
	sub.Attempts++
	sub.UpdatedAt = g.now()
	cur, ok, err = g.store.CompareAndSwap(ctx, id, Claimed, cur.Version, Submitting, sub)
	if err != nil {
		// Still Claimed (nothing sent); Recover can return it to Ready.
		return cur, err
	}
	if !ok {
		// Cancelled or recovered while we held the claim: do not send.
		return cur, notReady(cur)
	}

	// 3. The side effect. From here on it may have happened.
	res, sendErr := callSend(ctx, send)

	final := cur
	final.UpdatedAt = g.now()
	var to State
	var outErr error
	switch {
	case sendErr == nil:
		to = Succeeded
		final.Result = res
	case isDefinite(sendErr):
		to = Failed
		final.LastError = sendErr.Error()
		outErr = fmt.Errorf("%w: %w", ErrRejected, sendErr)
	default:
		to = Unknown
		final.LastError = sendErr.Error()
		outErr = fmt.Errorf("%w: %w", ErrUnknown, sendErr)
	}

	// 4. Persist the outcome even if ctx was cancelled meanwhile.
	pctx := context.WithoutCancel(ctx)
	rec, ok, err := g.store.CompareAndSwap(pctx, id, Submitting, cur.Version, to, final)
	if err != nil {
		return rec, errors.Join(fmt.Errorf("sendguard: persisting %q: %w", to, err), outErr)
	}
	if !ok {
		return rec, errors.Join(fmt.Errorf("%w: wanted %q, record is %q", ErrLostUpdate, to, rec.State), outErr)
	}
	return rec, outErr
}

func isDefinite(err error) bool {
	var d Definite
	return errors.As(err, &d) && d.Definite()
}

func callSend(ctx context.Context, send SendFunc) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			res, err = Result{}, &PanicError{Value: p}
		}
	}()
	return send(ctx)
}

// Outcome is the verdict of a manual or automated reconciliation.
type Outcome struct {
	// State must be Succeeded, Failed or Ready. Ready means "verified that
	// the external system did not receive it" and re-arms the record.
	State State
	// Result is stored when State is Succeeded.
	Result Result
	// Note is stored in LastError for Failed / Ready (audit trail).
	Note string
}

// Resolve moves an Unknown record to the reconciled outcome. It is the only
// way out of Unknown.
func (g *Guard) Resolve(ctx context.Context, id string, out Outcome) (Record, error) {
	if out.State != Succeeded && out.State != Failed && out.State != Ready {
		return Record{}, fmt.Errorf("sendguard: cannot resolve to %q", out.State)
	}
	cur, err := g.store.Get(ctx, id)
	if err != nil {
		return cur, err
	}
	if cur.State != Unknown {
		return cur, notReady(cur)
	}
	next := cur
	next.UpdatedAt = g.now()
	if out.State == Succeeded {
		next.Result = out.Result
	} else {
		next.LastError = out.Note
	}
	rec, ok, err := g.store.CompareAndSwap(ctx, id, Unknown, cur.Version, out.State, next)
	if err != nil {
		return rec, err
	}
	if !ok {
		return rec, notReady(rec)
	}
	return rec, nil
}

// Cancel moves a Ready or Claimed record to Cancelled. A record that is
// already Submitting cannot be cancelled.
func (g *Guard) Cancel(ctx context.Context, id string) (Record, error) {
	cur, err := g.store.Get(ctx, id)
	if err != nil {
		return cur, err
	}
	if cur.State != Ready && cur.State != Claimed {
		return cur, notReady(cur)
	}
	next := cur
	next.UpdatedAt = g.now()
	rec, ok, err := g.store.CompareAndSwap(ctx, id, cur.State, cur.Version, Cancelled, next)
	if err != nil {
		return rec, err
	}
	if !ok {
		return rec, notReady(rec)
	}
	return rec, nil
}

// Recover handles records abandoned by a crashed or stuck worker. If the
// record has not been updated for at least staleAfter:
//
//   - Claimed → Ready (the send function was never invoked);
//   - Submitting → Unknown (it may have been invoked).
//
// Any other record is returned unchanged with a nil error. Choose
// staleAfter well above the longest legitimate send duration: a slow but
// alive worker whose record is recovered gets [ErrLostUpdate], and its
// result has to be reconciled.
func (g *Guard) Recover(ctx context.Context, id string, staleAfter time.Duration) (Record, error) {
	cur, err := g.store.Get(ctx, id)
	if err != nil {
		return cur, err
	}
	var to State
	switch cur.State {
	case Claimed:
		to = Ready
	case Submitting:
		to = Unknown
	default:
		return cur, nil
	}
	now := g.now()
	if now.Sub(cur.UpdatedAt) < staleAfter {
		return cur, nil
	}
	next := cur
	next.UpdatedAt = now
	if to == Unknown {
		next.LastError = "recovered: worker did not report an outcome"
	}
	rec, ok, err := g.store.CompareAndSwap(ctx, id, cur.State, cur.Version, to, next)
	if err != nil {
		return rec, err
	}
	if !ok {
		return rec, notReady(rec)
	}
	return rec, nil
}
