package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderJSON encodes a value canonically so a test can compare a decoded dict
// against an expected shape in one string comparison. Go's encoder sorts map
// keys, which makes the result stable.
func renderJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling %#v: %v", v, err)
	}
	return string(data)
}

func TestLoadAdditionalParametersNone(t *testing.T) {
	// Nil rather than an empty map: the marshaler treats an empty overlay as no
	// overlay, and nil is how "no values given" is said without allocating.
	got, err := loadAdditionalParameters(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %#v, want nil for no flag values", got)
	}
}

func TestLoadAdditionalParametersInline(t *testing.T) {
	got, err := loadAdditionalParameters([]string{`{"a":1,"b":"two"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s := renderJSON(t, got); s != `{"a":1,"b":"two"}` {
		t.Errorf("got %s, want the dict as written", s)
	}
}

// TestLoadAdditionalParametersNumbersAreVerbatim verifies a numeric literal
// survives unchanged.
//
// The substance is the large integer: decoding into an `any` without UseNumber
// yields a float64, which cannot hold this value exactly and re-renders in
// exponent form. A caller passing an ID or a byte count would see it altered.
func TestLoadAdditionalParametersNumbersAreVerbatim(t *testing.T) {
	got, err := loadAdditionalParameters([]string{`{"big":123456789012345678,"one":1,"frac":1.50}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s := renderJSON(t, got); s != `{"big":123456789012345678,"frac":1.50,"one":1}` {
		t.Errorf("got %s; numeric literals must pass through as written", s)
	}
}

func TestLoadAdditionalParametersFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "extra.json")
	if err := os.WriteFile(path, []byte(`{"resources":{"limits":{"cpu":"2"}}}`), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	got, err := loadAdditionalParameters([]string{path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s := renderJSON(t, got); s != `{"resources":{"limits":{"cpu":"2"}}}` {
		t.Errorf("got %s, want the file's contents", s)
	}
}

// TestLoadAdditionalParametersMergesLastWins verifies repeated values merge, and
// that a shared key takes the later value whole.
//
// The "nested" key is the assertion that matters: a deep merge would produce
// {"a":1,"b":2} there. Shallow replacement is what lets a caller *replace* a
// structure an earlier file set, rather than only add to it.
func TestLoadAdditionalParametersMergesLastWins(t *testing.T) {
	got, err := loadAdditionalParameters([]string{
		`{"first":1,"shared":"early","nested":{"a":1}}`,
		`{"second":2,"shared":"late","nested":{"b":2}}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = `{"first":1,"nested":{"b":2},"second":2,"shared":"late"}`
	if s := renderJSON(t, got); s != want {
		t.Errorf("got %s, want %s", s, want)
	}
}

// TestLoadAdditionalParametersMixesInlineAndFile verifies the two forms merge
// with each other under the same last-wins rule.
func TestLoadAdditionalParametersMixesInlineAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.json")
	if err := os.WriteFile(path, []byte(`{"fromFile":true,"shared":"file"}`), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	got, err := loadAdditionalParameters([]string{path, `{"shared":"inline"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s := renderJSON(t, got); s != `{"fromFile":true,"shared":"inline"}` {
		t.Errorf("got %s, want the inline value to win over the file's", s)
	}
}

// TestLoadAdditionalParametersLeadingBraceIsInline verifies classification is by
// the leading character and not by what exists on disk.
//
// A file whose *name* is a JSON document is created here deliberately. Probing
// the filesystem first would read it and the assertion below would see
// "fromFile"; sniffing '{' means the value the user typed is the document,
// whatever the directory happens to contain.
func TestLoadAdditionalParametersLeadingBraceIsInline(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	const name = `{"a":1}`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(`{"fromFile":true}`), 0o600); err != nil {
		t.Skipf("this platform cannot create a file named %q: %v", name, err)
	}

	got, err := loadAdditionalParameters([]string{name})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s := renderJSON(t, got); s != `{"a":1}` {
		t.Errorf("got %s; a value starting with '{' is inline JSON, not a filename", s)
	}
}

// TestLoadAdditionalParametersLeadingWhitespace verifies an inline dict is
// recognized despite surrounding whitespace, which a shell heredoc or a
// copy-paste readily introduces.
func TestLoadAdditionalParametersLeadingWhitespace(t *testing.T) {
	got, err := loadAdditionalParameters([]string{"  \n\t{\"a\":1}\n  "})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s := renderJSON(t, got); s != `{"a":1}` {
		t.Errorf("got %s, want the surrounding whitespace ignored", s)
	}
}

func TestLoadAdditionalParametersErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	badJSONFile := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badJSONFile, []byte(`{"a":`), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	arrayFile := filepath.Join(t.TempDir(), "array.json")
	if err := os.WriteFile(arrayFile, []byte(`[1,2]`), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	for _, tc := range []struct {
		name  string
		value string
		want  string // substring the error must contain
	}{
		// A bare word is a filename, so the error names the missing file rather
		// than reporting a JSON syntax problem in text the user meant as a path.
		{"missing file", missing, "nope.json"},
		{"bare word", "notjson", "notjson"},
		{"empty value", "", "must not be empty"},
		{"whitespace only", "   ", "must not be empty"},
		{"truncated inline", `{"a":`, "invalid JSON"},
		{"truncated file", badJSONFile, "invalid JSON"},
		// An array has no names to merge by, so it is refused rather than dropped.
		{"array file", arrayFile, "expected a JSON object"},
		// A duplicate key in one document is a mistake, not a layering intent.
		{"duplicate key", `{"a":1,"a":2}`, `duplicate key "a"`},
		// Trailing content means the value was not the single dict it appeared to
		// be; ignoring it would silently drop the second half.
		{"trailing content", `{"a":1} {"b":2}`, "trailing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAdditionalParameters([]string{tc.value})
			if err == nil {
				t.Fatalf("expected an error for %q", tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			// Every error identifies the flag, so a user with several flags on the
			// command line knows which one to fix.
			if !strings.Contains(err.Error(), additionalParameterFlagName) {
				t.Errorf("error = %v, want it to name --%s", err, additionalParameterFlagName)
			}
		})
	}
}

// TestLoadAdditionalParametersErrorStopsAtFirstBadValue verifies a later bad
// value fails the whole call rather than the good values being used.
func TestLoadAdditionalParametersErrorStopsAtFirstBadValue(t *testing.T) {
	got, err := loadAdditionalParameters([]string{`{"a":1}`, `{"b":`})
	if err == nil {
		t.Fatal("expected an error for the second value")
	}
	if got != nil {
		t.Errorf("got %#v, want nil; a partial merge must not be returned", got)
	}
}
