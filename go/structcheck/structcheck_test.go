package structcheck_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/caprico99278/FledgeFlow-oss/go/structcheck"
)

// draftSchema is a typical "generated text" structured output.
var draftSchema = &structcheck.Schema{
	Type:     structcheck.Object,
	Required: []string{"title", "body", "tags", "language"},
	Properties: map[string]*structcheck.Schema{
		"title":    {Type: structcheck.String, MinLength: 1, MaxLength: 40},
		"body":     {Type: structcheck.String, MinLength: 1, MaxLength: 500},
		"tags":     {Type: structcheck.ArrayOfString, MaxItems: 5, MaxLength: 20, Pattern: `^[\p{L}\p{N}_]+$`},
		"language": {Type: structcheck.String, Enum: []string{"ja", "en"}},
		"score":    {Type: structcheck.Number},
		"rank":     {Type: structcheck.Integer},
		"nsfw":     {Type: structcheck.Boolean},
		"extra":    {Type: structcheck.Any},
		"meta": {
			Type:                 structcheck.Object,
			AdditionalProperties: true,
			Properties: map[string]*structcheck.Schema{
				"model": {Type: structcheck.String},
			},
		},
	},
}

func decode(t testing.TB, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func codes(vs []structcheck.Violation) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Path + " " + v.Code
	}
	return out
}

const validDoc = `{
	"title": "夕暮れの習作",
	"body": "A short description.",
	"tags": ["watercolor", "習作", "sky_01"],
	"language": "ja",
	"score": 0.75,
	"rank": 3,
	"nsfw": false,
	"extra": {"anything": [1, 2, 3]},
	"meta": {"model": "m-1", "free": true}
}`

func TestValid(t *testing.T) {
	if err := structcheck.Check(draftSchema); err != nil {
		t.Fatal(err)
	}
	if vs := structcheck.Validate(draftSchema, decode(t, validDoc)); vs != nil {
		t.Fatalf("unexpected violations: %v", vs)
	}
}

func TestViolationsAreDeterministic(t *testing.T) {
	doc := `{
		"zeta": 1,
		"alpha": 2,
		"title": "",
		"tags": ["ok", 7, "has space", "x", "y", "z"],
		"language": "fr",
		"score": "high",
		"rank": 1.5,
		"nsfw": "no",
		"meta": "oops"
	}`
	want := []string{
		"$.body required",
		"$.language enum",
		"$.meta type",
		"$.nsfw type",
		"$.rank type",
		"$.score type",
		"$.tags max_items",
		"$.tags[1] type",
		"$.tags[2] pattern",
		"$.title min_length",
		"$.alpha additional_property",
		"$.zeta additional_property",
	}
	m := decode(t, doc)
	first := structcheck.Validate(draftSchema, m)
	if got := codes(first); !reflect.DeepEqual(got, want) {
		t.Fatalf("violations:\n got  %q\n want %q", got, want)
	}
	// Map iteration order is random; the output must not be.
	for range 50 {
		if again := structcheck.Validate(draftSchema, decode(t, doc)); !reflect.DeepEqual(again, first) {
			t.Fatalf("non-deterministic output:\n%v\n%v", again, first)
		}
	}
}

func TestLengthIsCountedInRunes(t *testing.T) {
	s := &structcheck.Schema{Type: structcheck.String, MinLength: 2, MaxLength: 3}
	cases := map[string][]string{
		"あいう":  nil, // 3 runes, 9 bytes
		"あいうえ": {"$ max_length"},
		"é":    {"$ min_length"},
		"👍🏽":   nil, // 2 code points
	}
	for in, want := range cases {
		if got := codes(structcheck.Validate(s, in)); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Errorf("%q: got %v want %v", in, got, want)
		}
	}
}

func TestRequiredAndNull(t *testing.T) {
	s := &structcheck.Schema{
		Type:       structcheck.Object,
		Required:   []string{"b", "a", "a"},
		Properties: map[string]*structcheck.Schema{"a": {Type: structcheck.String}, "b": {Type: structcheck.String}, "c": {Type: structcheck.String}},
	}
	got := codes(structcheck.Validate(s, map[string]any{"a": nil, "c": nil}))
	want := []string{"$.a required", "$.b required"} // sorted, de-duplicated; optional null is fine
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNumbers(t *testing.T) {
	num := &structcheck.Schema{Type: structcheck.Number}
	integer := &structcheck.Schema{Type: structcheck.Integer}
	for _, v := range []any{1.5, 2.0, json.Number("3.25"), 7, int64(-2), uint8(1), float32(0.5)} {
		if vs := structcheck.Validate(num, v); vs != nil {
			t.Errorf("number %v (%T): %v", v, v, vs)
		}
	}
	for _, v := range []any{2.0, json.Number("10"), json.Number("1e3"), 7, int64(-2)} {
		if vs := structcheck.Validate(integer, v); vs != nil {
			t.Errorf("integer %v (%T): %v", v, v, vs)
		}
	}
	for _, v := range []any{1.5, json.Number("2.5"), "3", true, nil} {
		if vs := structcheck.Validate(integer, v); len(vs) != 1 || vs[0].Code != structcheck.CodeType {
			t.Errorf("integer %v (%T) should fail: %v", v, v, vs)
		}
	}
	// UseNumber decoding works too.
	dec := json.NewDecoder(strings.NewReader(`{"rank": 4, "score": 1.25}`))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	obj := &structcheck.Schema{Type: structcheck.Object, Properties: map[string]*structcheck.Schema{"rank": integer, "score": num}}
	if vs := structcheck.Validate(obj, m); vs != nil {
		t.Fatal(vs)
	}
}

func TestStringArrayVariants(t *testing.T) {
	s := &structcheck.Schema{Type: structcheck.ArrayOfString, MaxItems: 2, Enum: []string{"a", "b"}}
	if vs := structcheck.Validate(s, []string{"a", "b"}); vs != nil {
		t.Fatal(vs)
	}
	got := codes(structcheck.Validate(s, []any{"a", "c", "b"}))
	want := []string{"$ max_items", "$[1] enum"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := codes(structcheck.Validate(s, "a")); !reflect.DeepEqual(got, []string{"$ type"}) {
		t.Fatalf("got %v", got)
	}
}

func TestOddKeysAreQuotedInPaths(t *testing.T) {
	s := &structcheck.Schema{Type: structcheck.Object}
	vs := structcheck.Validate(s, map[string]any{"has space": 1, "9lives": 2, "ok_1": 3, "": 4})
	got := make([]string, len(vs))
	for i, v := range vs {
		got[i] = v.Path
	}
	want := []string{`$[""]`, `$["9lives"]`, `$["has space"]`, `$.ok_1`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNilSchemaAndHostileValues(t *testing.T) {
	if vs := structcheck.Validate(nil, map[string]any{"x": 1}); vs != nil {
		t.Fatal(vs)
	}
	// Unusual Go values must not panic.
	for _, v := range []any{nil, struct{}{}, make(chan int), []int{1}, map[int]int{1: 1}, func() {}} {
		_ = structcheck.Validate(draftSchema, v)
		_ = structcheck.Validate(draftSchema.Properties["tags"], v)
		_ = structcheck.Validate(draftSchema.Properties["score"], v)
	}
}

func TestCheckSchema(t *testing.T) {
	bad := &structcheck.Schema{
		Type: structcheck.Object,
		Properties: map[string]*structcheck.Schema{
			"a": {Type: "list"},
			"b": {Type: structcheck.String, Pattern: "("},
			"c": {Type: structcheck.Number, MaxLength: 3},
			"d": {Type: structcheck.String, MinLength: 5, MaxLength: 2},
			"e": {Type: structcheck.Boolean, MaxItems: 1},
		},
	}
	err := structcheck.Check(bad)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, frag := range []string{`$.a: unknown type "list"`, "$.b: pattern", "$.c: string constraints", "$.d: minLength 5 > maxLength 2", "$.e: maxItems"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error %q lacks %q", err, frag)
		}
	}
	// Invalid patterns surface as violations instead of panics.
	vs := structcheck.Validate(&structcheck.Schema{Type: structcheck.String, Pattern: "("}, "x")
	if len(vs) != 1 || vs[0].Code != structcheck.CodeInvalidSchema {
		t.Fatalf("got %v", vs)
	}
}

func BenchmarkValidateValid(b *testing.B) {
	m := decode(b, validDoc)
	b.ReportAllocs()
	for b.Loop() {
		if vs := structcheck.Validate(draftSchema, m); vs != nil {
			b.Fatal(vs)
		}
	}
}

func BenchmarkValidateInvalid(b *testing.B) {
	m := decode(b, `{"title":"","tags":["a b","c",1],"language":"xx","score":"s","zz":1}`)
	b.ReportAllocs()
	for b.Loop() {
		if vs := structcheck.Validate(draftSchema, m); len(vs) == 0 {
			b.Fatal("expected violations")
		}
	}
}

func ExampleValidate() {
	schema := &structcheck.Schema{
		Type:     structcheck.Object,
		Required: []string{"title", "tags"},
		Properties: map[string]*structcheck.Schema{
			"title": {Type: structcheck.String, MaxLength: 10},
			"tags":  {Type: structcheck.ArrayOfString, MaxItems: 2},
			"tone":  {Type: structcheck.String, Enum: []string{"calm", "bright"}},
		},
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(`{"title":"An overly long title","tags":["a","b","c"],"tone":"loud","x":1}`), &out)
	for _, v := range structcheck.Validate(schema, out) {
		fmt.Println(v.Path, v.Code)
	}
	// Output:
	// $.tags max_items
	// $.title max_length
	// $.tone enum
	// $.x additional_property
}

func ExampleCheck() {
	err := structcheck.Check(&structcheck.Schema{Type: structcheck.String, Pattern: "[unclosed"})
	fmt.Println(err != nil)
	// Output: true
}
