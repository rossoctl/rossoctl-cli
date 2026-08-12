package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAuthTokenPrintsExactlyTheToken is the test for the command's purpose:
// stdout must be the token and nothing else, because the output is destined for
// `$(rossoctl auth token)`.
//
// It asserts on the whole of stdout rather than with strings.Contains, which is
// the point — a label, a banner, or a second line would all satisfy "contains
// the token" while breaking every caller. The one permitted addition is the
// trailing newline, which command substitution strips.
func TestAuthTokenPrintsExactlyTheToken(t *testing.T) {
	isolateHome(t)
	const token = "opaque-token-value"
	loginWithToken(t, token)

	stdout, _, err := executeSplit(t, "auth", "token")
	if err != nil {
		t.Fatalf("auth token: %v", err)
	}

	if stdout != token+"\n" {
		t.Errorf("stdout = %q, want exactly %q", stdout, token+"\n")
	}
}

// TestAuthTokenWorksOnAnOpaqueToken pins that the token is not decoded. `login
// --token` accepts any string and an OAuth server may issue an opaque one, so a
// command that parsed the token as a JWT would fail on exactly the credentials
// that are hardest to get at any other way.
//
// The fixture is deliberately not JWS-shaped: no dots, so jwt.Decode would
// reject it.
func TestAuthTokenWorksOnAnOpaqueToken(t *testing.T) {
	isolateHome(t)
	const token = "not-a-jwt-at-all"
	loginWithToken(t, token)

	stdout, _, err := executeSplit(t, "auth", "token")
	if err != nil {
		t.Fatalf("auth token on an opaque token: %v", err)
	}
	if strings.TrimSpace(stdout) != token {
		t.Errorf("stdout = %q, want the opaque token %q printed as stored", stdout, token)
	}
}

// TestAuthTokenPrintsAnExpiredTokenUnchanged covers the other half of "not
// validated": an expired JWT is still the token the user asked for, and refusing
// to print it would leave them unable to reproduce the 401 they are debugging.
func TestAuthTokenPrintsAnExpiredTokenUnchanged(t *testing.T) {
	isolateHome(t)
	token := testToken(t, map[string]any{
		"preferred_username": "ada",
		"exp":                1,
	})
	loginWithToken(t, token)

	stdout, stderr, err := executeSplit(t, "auth", "token")
	if err != nil {
		t.Fatalf("auth token on an expired token: %v", err)
	}
	if strings.TrimSpace(stdout) != token {
		t.Errorf("stdout = %q, want the expired token printed unchanged", stdout)
	}
	// No warning either: stdout is machine-destined and stderr noise here would
	// be a surprise on every use of a token that is about to be refreshed
	// anyway. `auth status` is where expiry is reported.
	if strings.Contains(stderr, "WARNING") {
		t.Errorf("auth token should not warn about expiry; that is `auth status`: %q", stderr)
	}
}

// TestAuthTokenErrorsWithoutAToken is why the command exits non-zero rather than
// printing nothing: a script doing TOKEN=$(rossoctl auth token) must fail here,
// not send an empty Authorization header and get an opaque 401 from the server.
//
// Checking stdout is empty matters as much as the error: an error plus a blank
// line on stdout would still let `set -e`-less callers proceed with "".
func TestAuthTokenErrorsWithoutAToken(t *testing.T) {
	isolateHome(t)
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://dev/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	stdout, _, err := executeSplit(t, "auth", "token")
	if err == nil {
		t.Fatal("auth token on a context with no token should error")
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty so a caller cannot use a blank token", stdout)
	}
	// The message must name the context and the remedy, matching `auth status`.
	if !strings.Contains(err.Error(), `"dev"`) {
		t.Errorf("error %q should name the context", err)
	}
	if !strings.Contains(err.Error(), "rossoctl login") {
		t.Errorf("error %q should name `rossoctl login` as the remedy", err)
	}
}

// TestAuthTokenHonorsContextFlag verifies --context selects which token is
// printed. The tokens differ per context, so this fails if the command reads the
// current context regardless of the flag -- which would hand a caller the wrong
// credential silently, the worst failure this command has.
func TestAuthTokenHonorsContextFlag(t *testing.T) {
	isolateHome(t)
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://dev/api/v1/", "--bearer-token", "dev-token"); err != nil {
		t.Fatalf("create-context dev: %v", err)
	}
	// create-context makes prod current, so an unflagged run would print
	// prod-token; --context dev must override that.
	if _, err := execute(t, "config", "create-context",
		"--name", "prod", "--server", "http://prod/api/v1/", "--bearer-token", "prod-token"); err != nil {
		t.Fatalf("create-context prod: %v", err)
	}

	stdout, _, err := executeSplit(t, "--context", "dev", "auth", "token")
	if err != nil {
		t.Fatalf("auth token --context dev: %v", err)
	}
	if strings.TrimSpace(stdout) != "dev-token" {
		t.Errorf("stdout = %q, want dev-token from the --context override", stdout)
	}

	// And the current context without the flag, confirming the override was
	// doing the work above rather than dev happening to be current.
	stdout, _, err = executeSplit(t, "auth", "token")
	if err != nil {
		t.Fatalf("auth token: %v", err)
	}
	if strings.TrimSpace(stdout) != "prod-token" {
		t.Errorf("stdout = %q, want prod-token from the current context", stdout)
	}
}

// TestAuthTokenMakesNoNetworkCall pins the `auth` group's promise that these
// commands "do not contact the server". The context points at a server whose
// handler fails the test on any request.
func TestAuthTokenMakesNoNetworkCall(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("auth token contacted the server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", srv.URL+"/api/v1/", "--bearer-token", "tok"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	if out, err := execute(t, "auth", "token"); err != nil {
		t.Fatalf("auth token: %v\n%s", err, out)
	}
}

// TestAuthTokenRejectsArguments guards the shape of the command. Without
// cobra.NoArgs, `rossoctl auth token dev` would ignore the argument and print
// the current context's token -- someone reaching for a positional context name
// would get a token, just not the one they named.
func TestAuthTokenRejectsArguments(t *testing.T) {
	isolateHome(t)
	loginWithToken(t, "tok")

	if _, err := execute(t, "auth", "token", "dev"); err == nil {
		t.Error("auth token should reject positional arguments; use --context")
	}
}
