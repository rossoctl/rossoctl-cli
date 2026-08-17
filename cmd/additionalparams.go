package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"strings"
)

// additionalParameterFlagName is the flag whose values loadAdditionalParameters
// interprets. Named once so the errors below and the flag registrations in
// agents_import.go and tools_import.go cannot drift apart.
const additionalParameterFlagName = "additionalParameterJSON"

// loadAdditionalParameters interprets repeated --additionalParameterJSON values
// and merges them into the single dict that is overlaid onto a create request.
//
// Each value is either inline JSON or the name of a file containing JSON; see
// readAdditionalParameter. Every value must decode to a JSON object, because the
// result is merged into the request body by name — an array or a scalar has no
// names to merge.
//
// Merging is shallow and last-wins: a key present in two values takes the later
// value whole, rather than the two objects underneath being combined. So
// '{"resources":{"limits":{"cpu":"1"}}}' followed by
// '{"resources":{"requests":{"cpu":"1"}}}' sends only the requests, not both.
// The rule is the one mergeEnvVars applies to a repeated variable name, and it is
// the rule that lets a caller *replace* a nested structure the CLI or an earlier
// file already set; a deep merge could only ever add to it.
//
// Returns nil, not an empty map, when no values are given: the marshaler treats
// an empty overlay as no overlay, and nil says the same thing without allocating.
func loadAdditionalParameters(values []string) (map[string]any, error) {
	var merged map[string]any
	for _, v := range values {
		obj, err := readAdditionalParameter(v)
		if err != nil {
			return nil, err
		}
		if merged == nil {
			merged = make(map[string]any, len(obj))
		}
		maps.Copy(merged, obj)
	}
	return merged, nil
}

// readAdditionalParameter interprets one --additionalParameterJSON value as
// either inline JSON or a filename, and decodes it as a JSON object.
//
// The two are told apart by the first non-whitespace byte: '{' means the value
// is itself the document, anything else means it names a file. Sniffing the
// leading character rather than probing the filesystem keeps the meaning of a
// value a property of what the user typed, so a command does not change behavior
// because a file named '{"a":1}' happens to exist, and a misspelled filename
// reports that the file is missing instead of being parsed as JSON and reported
// as a syntax error.
//
// A value that is neither — a bare word, an array, a quoted string — is a
// missing file, and is reported as one.
func readAdditionalParameter(value string) (map[string]any, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, fmt.Errorf("--%s must not be empty", additionalParameterFlagName)
	}

	data := []byte(trimmed)
	source := "inline JSON"
	if !strings.HasPrefix(trimmed, "{") {
		contents, err := os.ReadFile(trimmed)
		if err != nil {
			return nil, fmt.Errorf("--%s %q: %w", additionalParameterFlagName, value, err)
		}
		data = contents
		source = fmt.Sprintf("file %q", trimmed)
	}

	obj, err := decodeJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("--%s: %s: %w", additionalParameterFlagName, source, err)
	}
	return obj, nil
}

// decodeJSONObject decodes data as a JSON object, rejecting a duplicate key and
// anything that is not an object.
//
// json.Unmarshal into a map would accept both: a duplicate name silently keeps
// the last value, and there would be no way to tell an object from any other
// value until the type assertion. A token-driven decode is used instead so
// '{"a":1,"a":2}' is refused rather than quietly becoming one of the two — a file
// naming the same parameter twice is a mistake worth reporting, and unlike a
// repeated *flag* (whose last-wins is a deliberate layering rule) nothing about
// one document expresses an ordering intent.
//
// UseNumber keeps numeric literals as json.Number, so a value passes through to
// the request body as written instead of being rendered from a float64 — 1 stays
// 1 rather than becoming 1e+00, and a large int64 keeps its digits.
func decodeJSONObject(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("expected a JSON object")
	}

	out := make(map[string]any)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("invalid JSON: expected an object key")
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var val any
		if err := dec.Decode(&val); err != nil {
			return nil, fmt.Errorf("invalid JSON for key %q: %w", key, err)
		}
		out[key] = val
	}
	// Consumes the closing '}' and confirms nothing follows it, so trailing
	// content after a complete object is an error rather than being ignored.
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("unexpected trailing content after the JSON object")
	}
	return out, nil
}
