package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// structured prepares a schema for a model that cannot be trusted with json_schema: the
// text that describes it field by field for the system prompt, and the schema compiled
// to check the answer with.
func structured(raw json.RawMessage) (prompt string, check *jsonschema.Resolved, err error) {
	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", nil, err
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", nil, err
	}
	if check, err = s.Resolve(nil); err != nil {
		return "", nil, err
	}
	return describe(root), check, nil
}

// check decodes text as exactly one JSON value and validates it against schema.
func check(schema *jsonschema.Resolved, text string) error {
	dec := json.NewDecoder(strings.NewReader(text))
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("the answer is not JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("the answer has text after the JSON")
	}
	if err := schema.Validate(v); err != nil {
		return fmt.Errorf("the answer does not fit the schema: %w", err)
	}
	return nil
}

// describe words a schema for a prompt: what the reply is, then every field with its
// type, whether it is required, and the nested fields under it.
func describe(root any) string {
	var b strings.Builder
	b.WriteString("Reply with a single JSON value and nothing else: no prose, no Markdown, no code fence.")
	s, _ := root.(map[string]any)
	if kind(s) == "object" && len(properties(s)) > 0 {
		b.WriteString(" It is an object with " + exactly(s) + " these fields:\n")
		fields(&b, s, 0)
	} else {
		b.WriteString(" The value is: " + phrase(s) + ".")
		if nested := nestedObject(s); nested != nil {
			who := "The object has"
			if kind(s) == "array" {
				who = "Each object in the array has"
			}
			b.WriteString(" " + who + " " + exactly(nested) + " these fields:\n")
			fields(&b, nested, 0)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// fields writes one line per field of the object s, required fields first in the order
// the schema lists them, then the rest by name.
func fields(b *strings.Builder, s map[string]any, depth int) {
	props := properties(s)
	required := requiredNames(s)
	names := make([]string, 0, len(props))
	for _, n := range required {
		if _, ok := props[n]; ok {
			names = append(names, n)
		}
	}
	rest := make([]string, 0, len(props))
	for n := range props {
		if !slices.Contains(names, n) {
			rest = append(rest, n)
		}
	}
	slices.Sort(rest)
	names = append(names, rest...)

	indent := strings.Repeat("  ", depth)
	for _, n := range names {
		fs, _ := props[n].(map[string]any)
		fmt.Fprintf(b, "%s- %q: %s", indent, n, phrase(fs))
		if slices.Contains(required, n) {
			b.WriteString(" (required)")
		} else {
			b.WriteString(" (optional)")
		}
		if d, _ := fs["description"].(string); d != "" {
			b.WriteString(", " + d)
		}
		if nested := nestedObject(fs); nested != nil {
			b.WriteString(", with " + exactly(nested) + " these fields:\n")
			fields(b, nested, depth+1)
			continue
		}
		b.WriteString("\n")
	}
}

// nestedObject is the object schema a field is, or the object its array holds.
func nestedObject(s map[string]any) map[string]any {
	if kind(s) == "object" && len(properties(s)) > 0 {
		return s
	}
	if kind(s) == "array" {
		if items, _ := s["items"].(map[string]any); kind(items) == "object" && len(properties(items)) > 0 {
			return items
		}
	}
	return nil
}

// phrase is the type of a value in words, with the constraints a reader needs.
func phrase(s map[string]any) string {
	for _, k := range []string{"$ref", "anyOf", "oneOf", "allOf", "not"} {
		if _, ok := s[k]; ok {
			raw, _ := json.Marshal(s)
			return "JSON that matches this schema: " + string(raw)
		}
	}
	var out string
	switch k := kind(s); k {
	case "array":
		items, _ := s["items"].(map[string]any)
		switch {
		case nestedObject(s) != nil:
			out = "array of objects"
		case items != nil:
			out = "array of " + phrase(items)
		default:
			out = "array"
		}
	case "":
		out = "any JSON value"
	default:
		out = k
	}
	if vals, ok := s["enum"].([]any); ok {
		raw, _ := json.Marshal(vals)
		out += ", one of " + string(raw)
	}
	if c, ok := s["const"]; ok {
		raw, _ := json.Marshal(c)
		out += ", always " + string(raw)
	}
	if p, _ := s["pattern"].(string); p != "" {
		out += ", matching the regular expression " + p
	}
	return out
}

// kind is the type of s: a JSON Schema type name, or the names of several joined by " or ".
func kind(s map[string]any) string {
	switch t := s["type"].(type) {
	case string:
		return t
	case []any:
		var names []string
		for _, n := range t {
			if name, ok := n.(string); ok {
				names = append(names, name)
			}
		}
		return strings.Join(names, " or ")
	}
	return ""
}

func properties(s map[string]any) map[string]any {
	p, _ := s["properties"].(map[string]any)
	return p
}

func requiredNames(s map[string]any) []string {
	var names []string
	list, _ := s["required"].([]any)
	for _, n := range list {
		if name, ok := n.(string); ok {
			names = append(names, name)
		}
	}
	return names
}

// exactly says whether an object may have fields beyond its listed ones.
func exactly(s map[string]any) string {
	if extra, ok := s["additionalProperties"].(bool); ok && !extra {
		return "exactly"
	}
	return "at least"
}
