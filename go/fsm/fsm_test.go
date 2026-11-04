package fsm_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/caprico99278/FledgeFlow-oss/go/fsm"
)

type light int

const (
	red light = iota
	green
	yellow
	off
)

func (l light) String() string { return [...]string{"red", "green", "yellow", "off"}[l] }

var traffic = fsm.Table[light]{
	red:    {green, off},
	green:  {yellow, off},
	yellow: {red, off},
}

func TestCan(t *testing.T) {
	cases := []struct {
		from, to light
		want     bool
	}{
		{red, green, true},
		{green, yellow, true},
		{yellow, red, true},
		{red, yellow, false},
		{red, red, false}, // self-transition not listed
		{off, red, false}, // terminal
		{green, off, true},
	}
	for _, c := range cases {
		if got := traffic.Can(c.from, c.to); got != c.want {
			t.Errorf("Can(%v, %v) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestCheckReturnsTypedError(t *testing.T) {
	if err := traffic.Check(red, green); err != nil {
		t.Fatalf("Check(red, green) = %v, want nil", err)
	}
	err := traffic.Check(off, green)
	var te *fsm.TransitionError[light]
	if !errors.As(err, &te) {
		t.Fatalf("Check(off, green) = %T %v, want *TransitionError[light]", err, err)
	}
	if te.From != off || te.To != green {
		t.Errorf("TransitionError = %+v", te)
	}
	if got, want := err.Error(), "fsm: transition off -> green is not allowed"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	// Wrapped errors still unwrap to the typed error.
	wrapped := fmt.Errorf("saving: %w", err)
	if !errors.As(wrapped, &te) {
		t.Error("errors.As through wrapping failed")
	}
}

func TestTerminalAndNext(t *testing.T) {
	if !traffic.Terminal(off) {
		t.Error("off should be terminal")
	}
	if traffic.Terminal(red) {
		t.Error("red should not be terminal")
	}
	explicit := fsm.Table[string]{"a": {"b"}, "b": {}}
	if !explicit.Terminal("b") || !explicit.Terminal("unknown") {
		t.Error("empty and missing entries are terminal")
	}
	next := traffic.Next(red)
	if !slices.Equal(next, []light{green, off}) {
		t.Errorf("Next(red) = %v", next)
	}
	next[0] = yellow // must not alias the table
	if traffic.Next(red)[0] != green {
		t.Error("Next returned an aliased slice")
	}
}

func TestZeroTable(t *testing.T) {
	var empty fsm.Table[int]
	if empty.Can(1, 2) || !empty.Terminal(1) {
		t.Error("nil table must allow nothing")
	}
}

func Example() {
	type state string
	order := fsm.Table[state]{
		"new":  {"paid", "cancelled"},
		"paid": {"shipped"},
	}
	fmt.Println(order.Can("new", "paid"))
	fmt.Println(order.Check("shipped", "new"))
	fmt.Println(order.Terminal("shipped"))
	// Output:
	// true
	// fsm: transition shipped -> new is not allowed
	// true
}

func ExampleTable_Check() {
	t := fsm.Table[string]{"draft": {"review"}, "review": {"approved", "draft"}}
	err := t.Check("draft", "approved")
	var te *fsm.TransitionError[string]
	if errors.As(err, &te) {
		fmt.Printf("rejected %s -> %s\n", te.From, te.To)
	}
	// Output: rejected draft -> approved
}
