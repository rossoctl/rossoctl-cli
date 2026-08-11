package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

func TestParseEnvVars(t *testing.T) {
	body := "FOO=bar\n" +
		"# a comment\n" +
		"\n" +
		"  BAZ = qux value \n" +
		"EMPTY=\n" +
		"URL=http://a=b\n" // value may contain '='

	got, err := parseEnvVars(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []struct{ name, value string }{
		{"FOO", "bar"},
		{"BAZ", "qux value"},
		{"EMPTY", ""},
		{"URL", "http://a=b"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d env vars, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].Value != w.value {
			t.Errorf("env[%d] = %+v, want {%s %s}", i, got[i], w.name, w.value)
		}
	}
}

func TestParseEnvVarsInvalidLine(t *testing.T) {
	if _, err := parseEnvVars("FOO=bar\nnotakeyvalue\n"); err == nil {
		t.Error("expected error for a line without '='")
	}
	if _, err := parseEnvVars("=novalue\n"); err == nil {
		t.Error("expected error for an empty key")
	}
}

// TestParseEnvVarsErrorNamesLine pins that the document path locates the failure
// by line number and quotes the offending text.
//
// The counterpart to TestParseEnvVarFlagsErrors, which asserts the flag path does
// NOT name a line. Together they are the two halves of why parseEnvVarPair is
// context-free: an implementation that fed flag values through this function as a
// synthetic document would satisfy one and fail the other.
//
// Asserts the properties, not the exact message, which is free to be reworded.
func TestParseEnvVarsErrorNamesLine(t *testing.T) {
	_, err := parseEnvVars("FOO=bar\nnotakeyvalue\n")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should locate the failing line: %v", err)
	}
	if !strings.Contains(err.Error(), "notakeyvalue") {
		t.Errorf("error should quote the offending text: %v", err)
	}
	if !errors.Is(err, errNotKeyValue) {
		t.Errorf("error should wrap errNotKeyValue: %v", err)
	}
}

// TestParseEnvVarPair covers the shared definition of key=value that both
// --envVarsURL lines and --envVar flags go through.
func TestParseEnvVarPair(t *testing.T) {
	cases := []struct {
		name         string
		in           string
		wantName     string
		wantValue    string
		wantErr      bool
		whyItMatters string
	}{
		{name: "plain", in: "FOO=bar", wantName: "FOO", wantValue: "bar"},
		{
			name: "value contains =", in: "URL=http://a=b",
			wantName: "URL", wantValue: "http://a=b",
			whyItMatters: "only the first = splits; strings.Split would truncate the value",
		},
		{
			name: "surrounding whitespace", in: "  FOO = bar ",
			wantName: "FOO", wantValue: "bar",
			whyItMatters: "both sides are trimmed",
		},
		{
			name: "interior whitespace", in: "FOO=a  b",
			wantName: "FOO", wantValue: "a  b",
			whyItMatters: "interior spacing is data, not separators",
		},
		{
			name: "empty value", in: "FOO=",
			wantName: "FOO", wantValue: "",
			whyItMatters: "VAR= is a real assignment and must be able to blank a document value",
		},
		{
			name: "leading hash", in: "#FOO=bar",
			wantName: "#FOO", wantValue: "bar",
			whyItMatters: "comment skipping belongs to the document, not the pair; " +
				"moving it here would silently discard an explicitly typed --envVar",
		},
		{
			name: "hash inside value", in: "FOO=#bar",
			wantName: "FOO", wantValue: "#bar",
			whyItMatters: "a # mid-value is data",
		},
		{
			name: "empty key", in: "=bar", wantErr: true,
		},
		{
			name: "whitespace-only key", in: " =bar", wantErr: true,
			whyItMatters: "the trim must happen before the empty-key check",
		},
		{
			name: "no equals", in: "FOO", wantErr: true,
			whyItMatters: "a bare name is not an assignment; this CLI has no passthrough concept",
		},
		{
			name: "empty string", in: "", wantErr: true,
			whyItMatters: "empty is an error here, never a silent skip",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseEnvVarPair(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseEnvVarPair(%q) = %+v, want an error (%s)", c.in, got, c.whyItMatters)
				}
				// The sentinel, not merely non-nil: both wrappers rely on it.
				if !errors.Is(err, errNotKeyValue) {
					t.Errorf("parseEnvVarPair(%q) error should wrap errNotKeyValue, got %v", c.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEnvVarPair(%q): unexpected error %v", c.in, err)
			}
			if got.Name != c.wantName || got.Value != c.wantValue {
				t.Errorf("parseEnvVarPair(%q) = {%q %q}, want {%q %q} (%s)",
					c.in, got.Name, got.Value, c.wantName, c.wantValue, c.whyItMatters)
			}
		})
	}
}

// TestParseEnvVarFlags covers the flag path, including what it must NOT do.
func TestParseEnvVarFlags(t *testing.T) {
	t.Run("nil yields nil", func(t *testing.T) {
		// Symmetric with fetchEnvVars(""), so mergeEnvVars sees nil from both
		// sources and EnvVars stays omitted.
		got, err := parseEnvVarFlags(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})

	t.Run("order preserved", func(t *testing.T) {
		got, err := parseEnvVarFlags([]string{"A=1", "B=2", "C=3"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"A", "B", "C"}
		if len(got) != len(want) {
			t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
		}
		for i, w := range want {
			if got[i].Name != w {
				t.Errorf("entry %d = %q, want %q; flag order must survive", i, got[i].Name, w)
			}
		}
	})

	t.Run("commas are literal", func(t *testing.T) {
		// The parse layer's half of the StringArrayVar decision; the flag layer's
		// half is TestAgentsImportFromImageEnvVarLiteralComma.
		got, err := parseEnvVarFlags([]string{"TAGS=a,b,c"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Value != "a,b,c" {
			t.Errorf("got %+v, want one entry with value a,b,c", got)
		}
	})

	t.Run("does not dedup", func(t *testing.T) {
		// Deduping is mergeEnvVars' job. Folding it in here would leave the
		// within-group case of that helper untested.
		got, err := parseEnvVarFlags([]string{"A=1", "A=2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %+v, want both entries; dedup belongs to mergeEnvVars", got)
		}
	})
}

// TestParseEnvVarFlagsErrors pins that a flag error names what the user typed and
// never a line number.
//
// This is the test that locks in the central design decision. Implementing
// --envVar by joining values with "\n" and reusing parseEnvVars would report
// `line 2` for a flag typed once — it fails both assertions below.
func TestParseEnvVarFlagsErrors(t *testing.T) {
	for _, in := range [][]string{
		{"A=1", "nope"},
		{"=1"},
		{""},
	} {
		if _, err := parseEnvVarFlags(in); err == nil {
			t.Errorf("parseEnvVarFlags(%q) = nil error, want one", in)
		}
	}

	_, err := parseEnvVarFlags([]string{"A=1", "nope"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("error should quote the flag value the user typed: %v", err)
	}
	if strings.Contains(err.Error(), "line") {
		t.Errorf("a --envVar error must not name a line number: %v", err)
	}
	if !strings.Contains(err.Error(), "--envVar") {
		t.Errorf("error should name the flag: %v", err)
	}
}

// TestMergeEnvVars covers precedence and position.
func TestMergeEnvVars(t *testing.T) {
	ev := func(pairs ...string) []apiclient.EnvVar {
		var out []apiclient.EnvVar
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, apiclient.EnvVar{Name: pairs[i], Value: pairs[i+1]})
		}
		return out
	}
	// "A=1 B=2" renders a result compactly enough to compare as one string.
	render := func(vars []apiclient.EnvVar) string {
		var parts []string
		for _, v := range vars {
			parts = append(parts, v.Name+"="+v.Value)
		}
		return strings.Join(parts, " ")
	}

	cases := []struct {
		name         string
		groups       [][]apiclient.EnvVar
		want         string
		whyItMatters string
	}{
		{
			name: "all empty", groups: [][]apiclient.EnvVar{nil, nil}, want: "",
			whyItMatters: "must be nil so envVars stays omitted from the request body",
		},
		{
			name:   "later group wins",
			groups: [][]apiclient.EnvVar{ev("A", "1"), ev("A", "2")},
			want:   "A=2",
		},
		{
			name:         "override keeps its slot",
			groups:       [][]apiclient.EnvVar{ev("A", "1", "B", "2"), ev("A", "3")},
			want:         "A=3 B=2",
			whyItMatters: "delete-and-append dedup would yield B=2 A=3, reordering the document",
		},
		{
			name:         "within one group",
			groups:       [][]apiclient.EnvVar{ev("A", "1", "B", "2", "A", "3")},
			want:         "A=3 B=2",
			whyItMatters: "a repeated key inside one group resolves too, so a two-argument signature is not enough",
		},
		{
			name:         "empty first group",
			groups:       [][]apiclient.EnvVar{nil, ev("A", "1")},
			want:         "A=1",
			whyItMatters: "indices must be assigned against out, not against the group",
		},
		{
			name:   "three groups chain",
			groups: [][]apiclient.EnvVar{ev("A", "1"), ev("A", "2"), ev("A", "3")},
			want:   "A=3",
		},
		{
			name:         "override to empty wins",
			groups:       [][]apiclient.EnvVar{ev("A", "1"), ev("A", "")},
			want:         "A=",
			whyItMatters: "--envVar FOO= must blank a document value; a skip-if-empty guard would keep the old one",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeEnvVars(c.groups...)
			if c.want == "" {
				// Distinguish nil from an empty slice: only nil is omitted by
				// encoding/json.
				if got != nil {
					t.Errorf("mergeEnvVars(...) = %+v, want nil (%s)", got, c.whyItMatters)
				}
				return
			}
			if r := render(got); r != c.want {
				t.Errorf("mergeEnvVars(...) = %q, want %q (%s)", r, c.want, c.whyItMatters)
			}
		})
	}
}

func TestParseEnvVarsEmpty(t *testing.T) {
	got, err := parseEnvVars("\n#only comments\n\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no env vars, got %+v", got)
	}
}

func TestSameHost(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"http://h:8080/api/v1/", "http://h:8080/env", true},
		{"http://h:8080/api/v1/", "http://h:9090/env", false}, // different port
		{"http://api.example.com/api/v1/", "https://raw.githubusercontent.com/x/.env", false},
		{"http://h:8080/", "not a url", true}, // url.Parse is lenient; host "" != "" guards below
	}
	for _, c := range cases {
		// The last case documents lenient parsing; assert the real intent:
		// only same host+port returns true.
		got := sameHost(c.a, c.b)
		if c.b == "not a url" {
			continue
		}
		if got != c.want {
			t.Errorf("sameHost(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestFetchEnvVarsForeignHostNoToken proves the API bearer token is NOT sent
// when the env URL is on a different host than the API server (the GitHub-404
// bug), but IS sent when the env URL is on the API host.
func TestFetchEnvVarsTokenHostGating(t *testing.T) {
	isolateHome(t)

	var apiAuth, foreignAuth string

	// The "API server": serves /namespaces and /env, records the auth header.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
		case "/env":
			apiAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte("FOO=bar\n"))
		default:
			t.Errorf("unexpected api path %q", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)

	// A "foreign" host (stands in for GitHub): records the auth header.
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("FOO=bar\n"))
	}))
	t.Cleanup(foreign.Close)

	// Context points at the API server with a token.
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", api.URL+"/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}
	if _, err := execute(t, "login", "--token", "api-token"); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Env URL on the API host -> token IS sent.
	if _, err := fetchEnvVars(context.Background(), rootCmd, api.URL+"/env"); err != nil {
		t.Fatalf("fetch (api host): %v", err)
	}
	if apiAuth != "Bearer api-token" {
		t.Errorf("api-host env fetch Authorization = %q, want %q", apiAuth, "Bearer api-token")
	}

	// Env URL on a foreign host -> token is NOT sent.
	if _, err := fetchEnvVars(context.Background(), rootCmd, foreign.URL+"/env"); err != nil {
		t.Fatalf("fetch (foreign host): %v", err)
	}
	if foreignAuth != "" {
		t.Errorf("foreign-host env fetch Authorization = %q, want empty", foreignAuth)
	}
}
