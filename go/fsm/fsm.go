// Package fsm provides a tiny, generic finite-state transition table.
//
// A [Table] maps each state to the states it may move to. It has no
// behaviour beyond answering "is this transition allowed?", which keeps it
// easy to embed in persistence code (for example inside a compare-and-swap
// guard) and easy to review: the whole lifecycle of an entity is a single
// map literal.
//
//	var Order = fsm.Table[string]{
//		"new":     {"paid", "cancelled"},
//		"paid":    {"shipped"},
//		"shipped": nil, // terminal
//	}
//	if err := Order.Check(cur, next); err != nil { ... }
package fsm

import "fmt"

// Table lists, for each state, the states it may transition to.
//
// States that appear only as targets (or are mapped to an empty slice) are
// terminal. A Table is safe for concurrent reads; do not modify it after
// it is shared.
type Table[S comparable] map[S][]S

// Can reports whether the transition from -> to is allowed.
// Self-transitions are allowed only when listed explicitly.
func (t Table[S]) Can(from, to S) bool {
	for _, s := range t[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Check returns nil if from -> to is allowed and a *[TransitionError]
// otherwise.
func (t Table[S]) Check(from, to S) error {
	if t.Can(from, to) {
		return nil
	}
	return &TransitionError[S]{From: from, To: to}
}

// Terminal reports whether s has no outgoing transitions.
func (t Table[S]) Terminal(s S) bool {
	return len(t[s]) == 0
}

// Next returns a copy of the states reachable from s in one step, in the
// order they were declared.
func (t Table[S]) Next(s S) []S {
	return append([]S(nil), t[s]...)
}

// TransitionError reports a transition that the table does not allow.
type TransitionError[S comparable] struct {
	From S
	To   S
}

// Error implements the error interface.
func (e *TransitionError[S]) Error() string {
	return fmt.Sprintf("fsm: transition %v -> %v is not allowed", e.From, e.To)
}
