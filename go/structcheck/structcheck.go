// Package structcheck validates decoded JSON values against a tiny
// declarative schema.
//
// It is intended for checking structured output produced by a language
// model (or any other untrusted producer) after json.Unmarshal into
// map[string]any: the schema is plain Go data, there is no dependency, and
// every problem is reported as a [Violation] with a stable machine-readable
// code, so callers can log, count or feed the violations back to the
// producer.
//
// Supported: object (with properties, required keys, additional-property
// control), string (enum, min/max length in runes, RE2 pattern), number,
// integer, boolean and array of strings (max items, per-item string rules).
// JSON null is treated as an absent property. Validation never panics on
// any input value.
//
// Violations are reported in a deterministic order: depth-first, object
// checks in this order — type, required keys (lexicographic), declared
// properties (lexicographic), additional properties (lexicographic) — and
// array items by index.
package structcheck

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Type is the JSON type a [Schema] accepts.
type Type string

// Supported types.
const (
	Any           Type = ""                // accept any value, including null
	String        Type = "string"          // Go string
	Number        Type = "number"          // float64, json.Number or any Go integer/float
	Integer       Type = "integer"         // a Number with no fractional part
	Boolean       Type = "boolean"         // Go bool
	ArrayOfString Type = "array-of-string" // []any whose items are strings, or []string
	Object        Type = "object"          // map[string]any
)

// Schema describes the expected shape of a value. Zero values of the
// numeric limits mean "no limit".
type Schema struct {
	Type Type

	// Object: declared properties.
	Properties map[string]*Schema
	// Object: keys that must be present (and not JSON null).
	Required []string
	// Object: allow keys that are not in Properties.
	AdditionalProperties bool

	// String (and each item of ArrayOfString): allowed values.
	Enum []string
	// String (and each item of ArrayOfString): length bounds in runes
	// (Unicode code points). 0 means no bound.
	MinLength int
	MaxLength int
	// String (and each item of ArrayOfString): RE2 regular expression that
	// must match somewhere in the value; anchor it with ^...$ for a full
	// match. Compiled once and cached.
	Pattern string

	// ArrayOfString: maximum number of items. 0 means no limit.
	MaxItems int
}

// Violation codes.
const (
	CodeType               = "type"
	CodeRequired           = "required"
	CodeAdditionalProperty = "additional_property"
	CodeEnum               = "enum"
	CodeMinLength          = "min_length"
	CodeMaxLength          = "max_length"
	CodeMaxItems           = "max_items"
	CodePattern            = "pattern"
	CodeInvalidSchema      = "invalid_schema"
	rootPath               = "$"
)

// Violation is one mismatch between a value and its schema.
type Violation struct {
	// Path locates the value: "$" is the root, "$.a.b[2]" a nested item;
	// keys that are not simple identifiers are quoted: $["odd key"].
	Path string
	// Code is one of the Code* constants.
	Code string
	// Message is a human-readable explanation (not part of the contract).
	Message string
}

func (v Violation) String() string { return v.Path + ": " + v.Code + ": " + v.Message }

// Validate checks value against s and returns all violations, or nil if
// the value conforms. A nil schema accepts everything.
func Validate(s *Schema, value any) []Violation {
	var c checker
	c.check(rootPath, s, value)
	return c.out
}

// Check reports problems in the schema itself (unknown type, invalid
// pattern, negative limits, constraints that cannot apply to the type).
// Validate also reports invalid patterns as violations, but calling Check
// once at start-up catches mistakes early.
func Check(s *Schema) error {
	var errs []string
	checkSchema(rootPath, s, &errs)
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("structcheck: invalid schema: %s", strings.Join(errs, "; "))
}

func checkSchema(path string, s *Schema, errs *[]string) {
	if s == nil {
		return
	}
	add := func(format string, args ...any) {
		*errs = append(*errs, path+": "+fmt.Sprintf(format, args...))
	}
	switch s.Type {
	case Any, String, Number, Integer, Boolean, ArrayOfString, Object:
	default:
		add("unknown type %q", s.Type)
	}
	if s.MinLength < 0 || s.MaxLength < 0 || s.MaxItems < 0 {
		add("negative limit")
	}
	if s.MaxLength > 0 && s.MinLength > s.MaxLength {
		add("minLength %d > maxLength %d", s.MinLength, s.MaxLength)
	}
	if s.Pattern != "" {
		if _, err := compile(s.Pattern); err != nil {
			add("pattern: %v", err)
		}
	}
	stringy := s.Type == String || s.Type == ArrayOfString
	if !stringy && (len(s.Enum) > 0 || s.MinLength != 0 || s.MaxLength != 0 || s.Pattern != "") {
		add("string constraints on type %q", s.Type)
	}
	if s.Type != ArrayOfString && s.MaxItems != 0 {
		add("maxItems on type %q", s.Type)
	}
	if s.Type != Object && (len(s.Properties) > 0 || len(s.Required) > 0) {
		add("object constraints on type %q", s.Type)
	}
	for _, k := range sortedKeys(s.Properties) {
		checkSchema(childPath(path, k), s.Properties[k], errs)
	}
}

var patternCache sync.Map // string -> *regexp.Regexp or error

func compile(p string) (*regexp.Regexp, error) {
	if v, ok := patternCache.Load(p); ok {
		if re, ok := v.(*regexp.Regexp); ok {
			return re, nil
		}
		return nil, v.(error)
	}
	re, err := regexp.Compile(p)
	if err != nil {
		patternCache.Store(p, err)
		return nil, err
	}
	patternCache.Store(p, re)
	return re, nil
}

type checker struct{ out []Violation }

func (c *checker) add(path, code, format string, args ...any) {
	c.out = append(c.out, Violation{Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
}

func (c *checker) check(path string, s *Schema, v any) {
	if s == nil {
		return
	}
	switch s.Type {
	case Any:
		return
	case String:
		str, ok := v.(string)
		if !ok {
			c.typeErr(path, s.Type, v)
			return
		}
		c.checkString(path, s, str)
	case Number:
		if _, ok := toFloat(v); !ok {
			c.typeErr(path, s.Type, v)
		}
	case Integer:
		if !isInteger(v) {
			c.typeErr(path, s.Type, v)
		}
	case Boolean:
		if _, ok := v.(bool); !ok {
			c.typeErr(path, s.Type, v)
		}
	case ArrayOfString:
		c.checkStringArray(path, s, v)
	case Object:
		c.checkObject(path, s, v)
	default:
		c.add(path, CodeInvalidSchema, "unknown schema type %q", s.Type)
	}
}

func (c *checker) typeErr(path string, want Type, v any) {
	c.add(path, CodeType, "expected %s, got %s", want, typeName(v))
}

func (c *checker) checkString(path string, s *Schema, str string) {
	if len(s.Enum) > 0 && !slices.Contains(s.Enum, str) {
		c.add(path, CodeEnum, "value %q is not one of %s", truncate(str, 40), quoteList(s.Enum))
	}
	if s.MinLength > 0 || s.MaxLength > 0 {
		n := utf8.RuneCountInString(str)
		if s.MinLength > 0 && n < s.MinLength {
			c.add(path, CodeMinLength, "length %d is less than %d", n, s.MinLength)
		}
		if s.MaxLength > 0 && n > s.MaxLength {
			c.add(path, CodeMaxLength, "length %d exceeds %d", n, s.MaxLength)
		}
	}
	if s.Pattern != "" {
		re, err := compile(s.Pattern)
		switch {
		case err != nil:
			c.add(path, CodeInvalidSchema, "invalid pattern: %v", err)
		case !re.MatchString(str):
			c.add(path, CodePattern, "value does not match %s", s.Pattern)
		}
	}
}

func (c *checker) checkStringArray(path string, s *Schema, v any) {
	var items []any
	switch a := v.(type) {
	case []any:
		items = a
	case []string:
		items = make([]any, len(a))
		for i, x := range a {
			items[i] = x
		}
	default:
		c.typeErr(path, ArrayOfString, v)
		return
	}
	if s.MaxItems > 0 && len(items) > s.MaxItems {
		c.add(path, CodeMaxItems, "%d items exceed %d", len(items), s.MaxItems)
	}
	for i, it := range items {
		ip := path + "[" + strconv.Itoa(i) + "]"
		str, ok := it.(string)
		if !ok {
			c.typeErr(ip, String, it)
			continue
		}
		c.checkString(ip, s, str)
	}
}

func (c *checker) checkObject(path string, s *Schema, v any) {
	obj, ok := v.(map[string]any)
	if !ok {
		c.typeErr(path, Object, v)
		return
	}
	req := slices.Clone(s.Required)
	slices.Sort(req)
	req = slices.Compact(req)
	for _, k := range req {
		if val, ok := obj[k]; !ok || val == nil {
			c.add(childPath(path, k), CodeRequired, "required property is missing")
		}
	}
	for _, k := range sortedKeys(s.Properties) {
		val, ok := obj[k]
		if !ok || val == nil {
			// Missing/null optional properties are fine; missing required
			// ones were reported above.
			continue
		}
		c.check(childPath(path, k), s.Properties[k], val)
	}
	if !s.AdditionalProperties {
		for _, k := range sortedKeys(obj) {
			if _, declared := s.Properties[k]; !declared {
				c.add(childPath(path, k), CodeAdditionalProperty, "property is not allowed")
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func isIdent(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func childPath(parent, key string) string {
	if isIdent(key) {
		return parent + "." + key
	}
	return parent + "[" + strconv.Quote(key) + "]"
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case float32:
		f := float64(n)
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	case json.Number:
		f, err := n.Float64()
		return f, err == nil && !math.IsInf(f, 0)
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

func isInteger(v any) bool {
	switch n := v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case json.Number:
		if _, err := n.Int64(); err == nil {
			return true
		}
	}
	f, ok := toFloat(v)
	return ok && f == math.Trunc(f)
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case []any, []string:
		return "array"
	}
	if _, ok := toFloat(v); ok {
		return "number"
	}
	return fmt.Sprintf("%T", v)
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func quoteList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = strconv.Quote(x)
	}
	return "[" + strings.Join(q, ", ") + "]"
}
