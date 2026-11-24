package jsonschema

import (
	"strings"
	"testing"
)

func mustCompile(t *testing.T, text string) *Schema {
	t.Helper()
	s, err := Compile(text)
	if err != nil {
		t.Fatalf("Compile(%s): %v", text, err)
	}
	return s
}

func check(t *testing.T, s *Schema, value string) error {
	t.Helper()
	v, err := Decode([]byte(value))
	if err != nil {
		t.Fatalf("Decode(%s): %v", value, err)
	}
	return s.Validate(v)
}

// A result's usual schema: an object with typed, required members.
func TestObjectSchema(t *testing.T) {
	s := mustCompile(t, `{
		"type": "object",
		"properties": {
			"name": {"type": "string", "minLength": 1},
			"count": {"type": "integer", "minimum": 0},
			"tags": {"type": "array", "items": {"type": "string"}, "uniqueItems": true},
			"status": {"enum": ["ok", "failed"]}
		},
		"required": ["name", "count"],
		"additionalProperties": false
	}`)
	for value, want := range map[string]string{
		`{"name": "x", "count": 3}`:                                     "",
		`{"name": "x", "count": 3, "tags": ["a", "b"], "status": "ok"}`: "",
		`{"name": "x"}`:                                 `has no "count", which the schema requires`,
		`{"name": "", "count": 1}`:                      "should be at least 1",
		`{"name": "x", "count": 1.5}`:                   "the schema wants integer",
		`{"name": "x", "count": -1}`:                    "-1 is less than 0",
		`{"name": "x", "count": 1, "tags": ["a", "a"]}`: "are the same",
		`{"name": "x", "count": 1, "tags": [1]}`:        "$.tags[0]: it is a number, and the schema wants string",
		`{"name": "x", "count": 1, "status": "maybe"}`:  "none of the values the schema allows",
		`{"name": "x", "count": 1, "extra": true}`:      `has "extra", which the schema does not allow`,
		`[1, 2]`: "it is a list, and the schema wants object",
	} {
		err := check(t, s, value)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", value, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: got %v, want %q", value, err, want)
		}
	}
}

// The combinators, $ref, patterns, and the other kinds of value.
func TestMoreKeywords(t *testing.T) {
	s := mustCompile(t, `{
		"$defs": {"id": {"type": "string", "pattern": "^[a-z]+-[0-9]+$"}},
		"type": "object",
		"properties": {
			"id": {"$ref": "#/$defs/id"},
			"either": {"anyOf": [{"type": "string"}, {"type": "null"}]},
			"one": {"oneOf": [{"type": "integer"}, {"type": "number", "minimum": 10}]},
			"pair": {"type": "array", "prefixItems": [{"type": "string"}, {"type": "boolean"}], "maxItems": 2},
			"notzero": {"not": {"const": 0}},
			"step": {"type": "number", "multipleOf": 0.5}
		}
	}`)
	for value, want := range map[string]string{
		`{"id": "ab-12", "either": null, "one": 3, "pair": ["x", true], "notzero": 1, "step": 2.5}`: "",
		`{"id": "AB-12"}`:              "does not match the pattern",
		`{"either": 3}`:                "matches none of the schemas anyOf lists",
		`{"one": 12}`:                  "matches 2 of the schemas oneOf lists",
		`{"pair": ["x", 1]}`:           "$.pair[1]: it is a number, and the schema wants boolean",
		`{"pair": ["x", true, false]}`: "should have at most 2",
		`{"notzero": 0}`:               "matches the schema not forbids",
		`{"step": 0.3}`:                "is not a multiple of 0.5",
	} {
		err := check(t, s, value)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", value, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: got %v, want %q", value, err, want)
		}
	}
}

// A schema that cannot be used is said so when it is compiled; a keyword
// gnosh does not know, and format, are left alone.
func TestCompileErrors(t *testing.T) {
	for text, want := range map[string]string{
		`not json`:                               "it is not JSON",
		`3`:                                      "a schema is an object or true or false",
		`{"type": "text"}`:                       "which is no JSON type",
		`{"type": "object", "required": "name"}`: "should be a list of names",
		`{"properties": {"a": 3}}`:               "a schema is an object or true or false",
		`{"pattern": "("}`:                       "cannot be read",
		`{"$ref": "https://example.com/s.json"}`: "only those are followed",
		`{"$ref": "#/$defs/missing"}`:            "leads nowhere",
		`{"minLength": -1}`:                      "a whole number",
		`{"anyOf": []}`:                          "a list of schemas",
		// Keywords that would hold an answer to more than is checked.
		`{"if": {"required": ["a"]}, "then": {"required": ["b"]}}`:            "a keyword gnosh does not check",
		`{"properties": {"l": {"contains": {"const": 1}, "minContains": 2}}}`: "a keyword gnosh does not check",
		`{"dependentRequired": {"a": ["b"]}}`:                                 "a keyword gnosh does not check",
		`{"type": "object", "unevaluatedProperties": false}`:                  "a keyword gnosh does not check",
	} {
		if _, err := Compile(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Compile(%s) = %v, want an error with %q", text, err, want)
		}
	}
	s := mustCompile(t, `{"type": "string", "format": "email", "x-own": {"anything": 1}}`)
	if err := check(t, s, `"not an address"`); err != nil {
		t.Errorf("format is enforced: %v", err)
	}
	if err := check(t, mustCompile(t, `true`), `{"a": 1}`); err != nil {
		t.Errorf("the schema true refused: %v", err)
	}
	if err := check(t, mustCompile(t, `false`), `1`); err == nil {
		t.Errorf("the schema false allowed a value")
	}
}

// contains asks that at least one item match the schema it holds.
func TestContains(t *testing.T) {
	t.Parallel()
	s := mustCompile(t, `{"type": "array", "contains": {"type": "string"}}`)
	for value, want := range map[string]string{
		`["a", 1]`: "",
		`[1, 2]`:   "no item matches the schema contains asks for",
		`[]`:       "no item matches the schema contains asks for",
	} {
		err := check(t, s, value)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", value, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: got %v, want %q", value, err, want)
		}
	}
}
