// Package jsonschema checks JSON values against a JSON Schema, for
// --json-schema: the run's structured output must conform to the schema
// the person gave.
//
// It knows the keywords a schema for a result is written with: type, enum
// and const; minimum, maximum, exclusiveMinimum, exclusiveMaximum and
// multipleOf; minLength, maxLength and pattern (Go's regular expressions);
// items (a schema, or a list as draft-07 writes a tuple), prefixItems,
// minItems, maxItems, contains and uniqueItems; properties, required,
// additionalProperties, patternProperties, propertyNames, minProperties
// and maxProperties; allOf, anyOf, oneOf and not; and $ref to a place in
// the same schema ("#", "#/$defs/name", "#/definitions/name"). format is
// accepted and not enforced, as Claude Code has it; keywords it does not
// know are left alone, as JSON Schema says, but for the ones that would
// hold an answer to more than it checks (if/then/else, minContains and
// maxContains, dependentRequired, dependentSchemas, dependencies,
// unevaluated*),
// which make the schema refused: an answer could pass without meeting
// them (the side-request review of 2026-11-19). A known keyword whose
// value has the wrong shape makes the schema invalid.
package jsonschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Schema is a compiled schema.
type Schema struct {
	root     any
	patterns map[string]*regexp.Regexp
}

// Compile reads a schema and checks that it can be used: valid JSON, an
// object or a boolean, every known keyword of the right shape, every
// pattern a regular expression, every $ref one this package can follow.
func Compile(text string) (*Schema, error) {
	root, err := Decode([]byte(text))
	if err != nil {
		return nil, fmt.Errorf("it is not JSON: %v", err)
	}
	s := &Schema{root: root, patterns: map[string]*regexp.Regexp{}}
	if err := s.check(root, "#"); err != nil {
		return nil, err
	}
	return s, nil
}

// Decode reads JSON with its numbers kept as written (json.Number).
func Decode(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("there is more after the value")
	}
	return v, nil
}

var typeNames = map[string]bool{"null": true, "boolean": true, "object": true, "array": true, "number": true, "integer": true, "string": true}

// check walks a schema and says what is wrong with it.
func (s *Schema) check(node any, at string) error {
	if _, ok := node.(bool); ok {
		return nil
	}
	m, ok := node.(map[string]any)
	if !ok {
		return fmt.Errorf("%s is %s, and a schema is an object or true or false", at, kindOf(node))
	}
	bad := func(key, want string) error {
		return fmt.Errorf("%s/%s should be %s", at, key, want)
	}
	for key, v := range m {
		switch key {
		case "if", "then", "else", "minContains", "maxContains", "dependentRequired", "dependentSchemas", "dependencies", "unevaluatedProperties", "unevaluatedItems":
			return fmt.Errorf("%s/%s is a keyword gnosh does not check, so an answer could pass without meeting it: write the schema without it", at, key)
		case "type":
			switch t := v.(type) {
			case string:
				if !typeNames[t] {
					return fmt.Errorf("%s/type is %q, which is no JSON type", at, t)
				}
			case []any:
				for _, x := range t {
					if name, ok := x.(string); !ok || !typeNames[name] {
						return bad(key, "a type's name or a list of them")
					}
				}
			default:
				return bad(key, "a type's name or a list of them")
			}
		case "enum":
			if _, ok := v.([]any); !ok {
				return bad(key, "a list")
			}
		case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf":
			n, ok := number(v)
			if !ok || key == "multipleOf" && n <= 0 {
				return bad(key, "a number")
			}
		case "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties":
			if n, ok := number(v); !ok || n < 0 || n != math.Trunc(n) {
				return bad(key, "a whole number, 0 or more")
			}
		case "pattern":
			p, ok := v.(string)
			if !ok {
				return bad(key, "a regular expression")
			}
			re, err := regexp.Compile(p)
			if err != nil {
				return fmt.Errorf("%s/pattern cannot be read: %v", at, err)
			}
			s.patterns[p] = re
		case "required":
			list, ok := v.([]any)
			if !ok {
				return bad(key, "a list of names")
			}
			for _, x := range list {
				if _, ok := x.(string); !ok {
					return bad(key, "a list of names")
				}
			}
		case "uniqueItems":
			if _, ok := v.(bool); !ok {
				return bad(key, "true or false")
			}
		case "items", "additionalProperties", "not", "propertyNames", "contains", "additionalItems":
			if list, ok := v.([]any); ok && key == "items" {
				for i, x := range list {
					if err := s.check(x, fmt.Sprintf("%s/items/%d", at, i)); err != nil {
						return err
					}
				}
				continue
			}
			if err := s.check(v, at+"/"+key); err != nil {
				return err
			}
		case "properties", "patternProperties", "$defs", "definitions":
			props, ok := v.(map[string]any)
			if !ok {
				return bad(key, "an object of schemas")
			}
			for name, x := range props {
				if key == "patternProperties" {
					re, err := regexp.Compile(name)
					if err != nil {
						return fmt.Errorf("%s/patternProperties has %q, which cannot be read: %v", at, name, err)
					}
					s.patterns[name] = re
				}
				if err := s.check(x, at+"/"+key+"/"+name); err != nil {
					return err
				}
			}
		case "allOf", "anyOf", "oneOf", "prefixItems":
			list, ok := v.([]any)
			if !ok || len(list) == 0 && key != "prefixItems" {
				return bad(key, "a list of schemas")
			}
			for i, x := range list {
				if err := s.check(x, fmt.Sprintf("%s/%s/%d", at, key, i)); err != nil {
					return err
				}
			}
		case "$ref":
			ref, ok := v.(string)
			if !ok {
				return bad(key, "a string")
			}
			if _, err := s.resolve(ref); err != nil {
				return fmt.Errorf("%s/$ref: %v", at, err)
			}
		}
	}
	return nil
}

// resolve follows a $ref inside the schema.
func (s *Schema) resolve(ref string) (any, error) {
	if ref == "#" {
		return s.root, nil
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("%q is not a place in this schema, and only those are followed", ref)
	}
	node := s.root
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		m, ok := node.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%q leads nowhere", ref)
		}
		if node, ok = m[part]; !ok {
			return nil, fmt.Errorf("%q leads nowhere", ref)
		}
	}
	return node, nil
}

// Validate checks a value (as Decode gives it) against the schema and
// says, for the first few places where it does not conform, what is
// wrong.
func (s *Schema) Validate(v any) error {
	var errs []string
	s.validate(s.root, v, "$", &errs, 0)
	if len(errs) == 0 {
		return nil
	}
	if len(errs) > 5 {
		errs = append(errs[:5], fmt.Sprintf("and %d more", len(errs)-5))
	}
	return errors.New(strings.Join(errs, "; "))
}

// validate adds to errs what is wrong with v at path under the schema node.
func (s *Schema) validate(node, v any, path string, errs *[]string, depth int) {
	fail := func(format string, args ...any) {
		*errs = append(*errs, path+": "+fmt.Sprintf(format, args...))
	}
	if depth > 64 {
		fail("the schema goes deeper than gnosh follows")
		return
	}
	switch b := node.(type) {
	case bool:
		if !b {
			fail("no value is allowed here")
		}
		return
	case map[string]any:
	default:
		return
	}
	m := node.(map[string]any)
	if ref, ok := m["$ref"].(string); ok {
		if target, err := s.resolve(ref); err == nil {
			s.validate(target, v, path, errs, depth+1)
		}
	}
	if t, ok := m["type"]; ok && !typeMatches(t, v) {
		fail("it is %s, and the schema wants %s", kindOf(v), typeWords(t))
		return
	}
	if list, ok := m["enum"].([]any); ok {
		found := false
		for _, x := range list {
			found = found || equal(x, v)
		}
		if !found {
			fail("it is none of the values the schema allows")
		}
	}
	if c, ok := m["const"]; ok && !equal(c, v) {
		fail("it is not the one value the schema allows")
	}
	if n, ok := number(v); ok && isNumber(v) {
		if lo, ok := number(m["minimum"]); ok && n < lo {
			fail("%s is less than %s", numText(v), fmtNum(lo))
		}
		if hi, ok := number(m["maximum"]); ok && n > hi {
			fail("%s is more than %s", numText(v), fmtNum(hi))
		}
		if lo, ok := number(m["exclusiveMinimum"]); ok && n <= lo {
			fail("%s is not more than %s", numText(v), fmtNum(lo))
		}
		if hi, ok := number(m["exclusiveMaximum"]); ok && n >= hi {
			fail("%s is not less than %s", numText(v), fmtNum(hi))
		}
		if k, ok := number(m["multipleOf"]); ok && k > 0 {
			if q := n / k; math.Abs(q-math.Round(q)) > 1e-9 {
				fail("%s is not a multiple of %s", numText(v), fmtNum(k))
			}
		}
	}
	if str, ok := v.(string); ok {
		n := utf8.RuneCountInString(str)
		if lo, ok := number(m["minLength"]); ok && float64(n) < lo {
			fail("it is %d characters long, and should be at least %s", n, fmtNum(lo))
		}
		if hi, ok := number(m["maxLength"]); ok && float64(n) > hi {
			fail("it is %d characters long, and should be at most %s", n, fmtNum(hi))
		}
		if p, ok := m["pattern"].(string); ok {
			if re := s.patterns[p]; re != nil && !re.MatchString(str) {
				fail("it does not match the pattern %s", p)
			}
		}
	}
	if list, ok := v.([]any); ok {
		s.validateArray(m, list, path, errs, depth)
	}
	if obj, ok := v.(map[string]any); ok {
		s.validateObject(m, obj, path, errs, depth)
	}
	if subs, ok := m["allOf"].([]any); ok {
		for _, sub := range subs {
			s.validate(sub, v, path, errs, depth+1)
		}
	}
	if subs, ok := m["anyOf"].([]any); ok {
		if matching(s, subs, v, depth) == 0 {
			fail("it matches none of the schemas anyOf lists")
		}
	}
	if subs, ok := m["oneOf"].([]any); ok {
		if n := matching(s, subs, v, depth); n != 1 {
			fail("it matches %d of the schemas oneOf lists, and should match one", n)
		}
	}
	if sub, ok := m["not"]; ok {
		var inner []string
		s.validate(sub, v, path, &inner, depth+1)
		if len(inner) == 0 {
			fail("it matches the schema not forbids")
		}
	}
}

func matching(s *Schema, subs []any, v any, depth int) int {
	n := 0
	for _, sub := range subs {
		var inner []string
		s.validate(sub, v, "$", &inner, depth+1)
		if len(inner) == 0 {
			n++
		}
	}
	return n
}

func (s *Schema) validateArray(m map[string]any, list []any, path string, errs *[]string, depth int) {
	fail := func(format string, args ...any) {
		*errs = append(*errs, path+": "+fmt.Sprintf(format, args...))
	}
	if lo, ok := number(m["minItems"]); ok && float64(len(list)) < lo {
		fail("it has %d items, and should have at least %s", len(list), fmtNum(lo))
	}
	if hi, ok := number(m["maxItems"]); ok && float64(len(list)) > hi {
		fail("it has %d items, and should have at most %s", len(list), fmtNum(hi))
	}
	if u, _ := m["uniqueItems"].(bool); u {
		for i := range list {
			for j := i + 1; j < len(list); j++ {
				if equal(list[i], list[j]) {
					fail("items %d and %d are the same, and the schema wants every item different", i, j)
				}
			}
		}
	}
	if sub, ok := m["contains"]; ok {
		found := false
		for _, x := range list {
			var inner []string
			s.validate(sub, x, path, &inner, depth+1)
			if len(inner) == 0 {
				found = true
				break
			}
		}
		if !found {
			fail("no item matches the schema contains wants")
		}
	}
	prefix, _ := m["prefixItems"].([]any)
	if tuple, ok := m["items"].([]any); ok {
		prefix = tuple // draft-07's way of writing a tuple
	}
	for i, x := range list {
		at := fmt.Sprintf("%s[%d]", path, i)
		switch {
		case i < len(prefix):
			s.validate(prefix[i], x, at, errs, depth+1)
		case m["items"] != nil && !isList(m["items"]):
			s.validate(m["items"], x, at, errs, depth+1)
		case isList(m["items"]) && m["additionalItems"] != nil:
			s.validate(m["additionalItems"], x, at, errs, depth+1)
		}
	}
}

func (s *Schema) validateObject(m map[string]any, obj map[string]any, path string, errs *[]string, depth int) {
	fail := func(format string, args ...any) {
		*errs = append(*errs, path+": "+fmt.Sprintf(format, args...))
	}
	if lo, ok := number(m["minProperties"]); ok && float64(len(obj)) < lo {
		fail("it has %d members, and should have at least %s", len(obj), fmtNum(lo))
	}
	if hi, ok := number(m["maxProperties"]); ok && float64(len(obj)) > hi {
		fail("it has %d members, and should have at most %s", len(obj), fmtNum(hi))
	}
	if req, ok := m["required"].([]any); ok {
		for _, x := range req {
			if name, _ := x.(string); name != "" {
				if _, ok := obj[name]; !ok {
					fail("it has no %q, which the schema requires", name)
				}
			}
		}
	}
	props, _ := m["properties"].(map[string]any)
	pats, _ := m["patternProperties"].(map[string]any)
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		at := path + "." + k
		known := false
		if sub, ok := props[k]; ok {
			known = true
			s.validate(sub, obj[k], at, errs, depth+1)
		}
		for p, sub := range pats {
			if re := s.patterns[p]; re != nil && re.MatchString(k) {
				known = true
				s.validate(sub, obj[k], at, errs, depth+1)
			}
		}
		if names, ok := m["propertyNames"]; ok {
			s.validate(names, k, at+" (its name)", errs, depth+1)
		}
		if !known {
			if extra, ok := m["additionalProperties"]; ok {
				if b, isBool := extra.(bool); isBool && !b {
					fail("it has %q, which the schema does not allow", k)
				} else if !isBool {
					s.validate(extra, obj[k], at, errs, depth+1)
				}
			}
		}
	}
}

// typeMatches says whether v is of the type (or one of the types) t names.
func typeMatches(t, v any) bool {
	names := []string{}
	switch x := t.(type) {
	case string:
		names = append(names, x)
	case []any:
		for _, n := range x {
			if s, ok := n.(string); ok {
				names = append(names, s)
			}
		}
	}
	for _, n := range names {
		switch n {
		case "null":
			if v == nil {
				return true
			}
		case "boolean":
			if _, ok := v.(bool); ok {
				return true
			}
		case "object":
			if _, ok := v.(map[string]any); ok {
				return true
			}
		case "array":
			if _, ok := v.([]any); ok {
				return true
			}
		case "string":
			if _, ok := v.(string); ok {
				return true
			}
		case "number":
			if isNumber(v) {
				return true
			}
		case "integer":
			if f, ok := number(v); ok && isNumber(v) && f == math.Trunc(f) {
				return true
			}
		}
	}
	return false
}

func typeWords(t any) string {
	switch x := t.(type) {
	case string:
		return x
	case []any:
		var out []string
		for _, n := range x {
			out = append(out, fmt.Sprint(n))
		}
		return strings.Join(out, " or ")
	}
	return "something else"
}

func isList(v any) bool {
	_, ok := v.([]any)
	return ok
}

func isNumber(v any) bool {
	switch v.(type) {
	case json.Number, float64, int, int64:
		return true
	}
	return false
}

// number reads a JSON number.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func numText(v any) string {
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	f, _ := number(v)
	return fmtNum(f)
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// equal compares two JSON values as JSON does: numbers by value, objects
// whatever the order of their members.
func equal(a, b any) bool {
	if fa, ok := number(a); ok && isNumber(a) {
		fb, ok := number(b)
		return ok && isNumber(b) && fa == fb
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !equal(xv, yv) {
				return false
			}
		}
		return true
	}
	return false
}

// kindOf names the kind of a JSON value.
func kindOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "true or false"
	case map[string]any:
		return "an object"
	case []any:
		return "a list"
	case string:
		return "a string"
	}
	if isNumber(v) {
		return "a number"
	}
	return "something else"
}
