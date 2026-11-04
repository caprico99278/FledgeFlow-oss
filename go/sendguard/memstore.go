package sendguard

import (
	"context"
	"fmt"
	"sync"
)

// MemStore is an in-memory reference [Store] guarded by a mutex. It is
// intended for tests and as an executable specification of the Store
// contract; production stores implement the same CAS with a conditional
// UPDATE (... WHERE id = ? AND state = ? AND version = ?).
//
// MemStore also rejects transitions not listed in [Transitions].
type MemStore struct {
	mu   sync.Mutex
	recs map[string]Record
}

// NewMemStore returns an empty store.
func NewMemStore() *MemStore {
	return &MemStore{recs: make(map[string]Record)}
}

// Get implements [Store].
func (m *MemStore) Get(ctx context.Context, id string) (Record, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.recs[id]
	if !ok {
		return Record{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	return rec, nil
}

// CompareAndSwap implements [Store].
func (m *MemStore) CompareAndSwap(ctx context.Context, id string, from State, fromVersion int64, to State, patch Record) (Record, bool, error) {
	if err := ctx.Err(); err != nil {
		return Record{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, exists := m.recs[id]
	if !exists {
		cur = Record{ID: id, State: None}
	}
	if cur.State != from || cur.Version != fromVersion {
		if !exists {
			return Record{}, false, nil
		}
		return cur, false, nil
	}
	if err := Transitions.Check(from, to); err != nil {
		return cur, false, fmt.Errorf("sendguard: %w", err)
	}
	next := patch
	next.ID = id
	next.State = to
	next.Version = fromVersion + 1
	m.recs[id] = next
	return next, true, nil
}

// Len returns the number of records (for tests and diagnostics).
func (m *MemStore) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.recs)
}
