package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/jwt"
)

// testToken builds a JWS-shaped token carrying claims. Only the payload matters:
// `auth status` decodes without verifying, so a placeholder signature is enough.
func testToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(payload) + ".c2ln"
}

// loginWithToken establishes a context named dev holding token.
func loginWithToken(t *testing.T, token string) {
	t.Helper()
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://dev/api/v1/", "--bearer-token", token); err != nil {
		t.Fatalf("create-context: %v", err)
	}
}

// TestAuthStatusReportsEveryRequestedClaim pins the reported set: the name,
// preferred_username, and email; the issuer; the expiration; the audiences; the
// realm_access roles; and the scopes.
func TestAuthStatusReportsEveryRequestedClaim(t *testing.T) {
	isolateHome(t)
	exp := time.Now().Add(2 * time.Hour)
	loginWithToken(t, testToken(t, map[string]any{
		"iss":                "https://keycloak.localtest.me/realms/rossoctl",
		"name":               "Ada Lovelace",
		"preferred_username": "ada",
		"email":              "ada@example.com",
		"aud":                []string{"rossoctl-ui", "account"},
		"exp":                exp.Unix(),
		"scope":              "openid profile email",
		"realm_access":       map[string]any{"roles": []string{"admin", "agent-user"}},
	}))

	out, err := execute(t, "auth", "status")
	if err != nil {
		t.Fatalf("auth status: %v\n%s", err, out)
	}

	for _, want := range []string{
		"Ada Lovelace",
		"ada",
		"ada@example.com",
		"https://keycloak.localtest.me/realms/rossoctl",
		exp.Format(time.RFC3339),
		"rossoctl-ui, account",
		"admin, agent-user",
		"openid, profile, email",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("auth status output missing %q:\n%s", want, out)
		}
	}

	// A live token must not be reported as expired.
	if strings.Contains(out, "WARNING") || strings.Contains(out, "EXPIRED") {
		t.Errorf("a token valid for two hours must not warn:\n%s", out)
	}
}

// TestAuthStatusWarnsOnAnExpiredToken is the point of the command: the warning
// must be present, and it must name the way to fix it.
func TestAuthStatusWarnsOnAnExpiredToken(t *testing.T) {
	isolateHome(t)
	exp := time.Now().Add(-30 * time.Minute)
	loginWithToken(t, testToken(t, map[string]any{
		"preferred_username": "ada",
		"exp":                exp.Unix(),
	}))

	out, err := execute(t, "auth", "status")
	if err != nil {
		t.Fatalf("auth status: %v\n%s", err, out)
	}

	if !strings.Contains(out, "WARNING") {
		t.Errorf("an expired token must warn:\n%s", out)
	}
	if !strings.Contains(out, "EXPIRED") {
		t.Errorf("the Expires row should mark the token expired:\n%s", out)
	}
	if !strings.Contains(out, "rossoctl login") {
		t.Errorf("the warning should name `rossoctl login` as the remedy:\n%s", out)
	}
	// An expired token is reported, not treated as a failure: the claims are
	// still what the user asked to see.
	if !strings.Contains(out, "ada") {
		t.Errorf("claims should still be shown for an expired token:\n%s", out)
	}
}

// TestAuthStatusOnATokenWithNoExpiry covers the absent-exp case, which must be
// said explicitly rather than left blank or warned about.
func TestAuthStatusOnATokenWithNoExpiry(t *testing.T) {
	isolateHome(t)
	loginWithToken(t, testToken(t, map[string]any{"preferred_username": "ada"}))

	out, err := execute(t, "auth", "status")
	if err != nil {
		t.Fatalf("auth status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "never") {
		t.Errorf("a token with no exp claim should say so:\n%s", out)
	}
	if strings.Contains(out, "WARNING") {
		t.Errorf("a token with no exp claim must not warn:\n%s", out)
	}
}

// TestAuthStatusMarksEmptyListsRatherThanOmittingThem covers the case that
// explains a 403: a token with no roles, audiences, or scopes.
func TestAuthStatusMarksEmptyListsRatherThanOmittingThem(t *testing.T) {
	isolateHome(t)
	loginWithToken(t, testToken(t, map[string]any{"preferred_username": "ada"}))

	out, err := execute(t, "auth", "status")
	if err != nil {
		t.Fatalf("auth status: %v\n%s", err, out)
	}
	for _, want := range []string{"Audiences", "Roles", "Scopes", "(none)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should still show %q for an empty claim:\n%s", want, out)
		}
	}
}

func TestAuthStatusJSON(t *testing.T) {
	isolateHome(t)
	loginWithToken(t, testToken(t, map[string]any{
		"preferred_username": "ada",
		"aud":                "rossoctl-ui",
		"realm_access":       map[string]any{"roles": []string{"admin"}},
	}))

	out, err := execute(t, "auth", "status", "--json")
	if err != nil {
		t.Fatalf("auth status --json: %v\n%s", err, out)
	}

	var got jwt.Claims
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out)
	}
	if got.Username != "ada" {
		t.Errorf("preferred_username = %q, want ada", got.Username)
	}
	if strings.Join(got.Audience, ",") != "rossoctl-ui" {
		t.Errorf("aud = %v", got.Audience)
	}
	if strings.Join(got.Roles(), ",") != "admin" {
		t.Errorf("roles = %v", got.Roles())
	}
}

// TestAuthStatusWithNoToken must point at login rather than printing an empty
// report.
func TestAuthStatusWithNoToken(t *testing.T) {
	isolateHome(t)
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://dev/api/v1/"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	_, err := execute(t, "auth", "status")
	if err == nil {
		t.Fatal("auth status on a context with no token should fail")
	}
	if !strings.Contains(err.Error(), "rossoctl login") {
		t.Errorf("error should name `rossoctl login`: %v", err)
	}
}

// TestAuthStatusWithAnOpaqueToken covers `login --token`, which accepts any
// string: the failure must explain that only JWTs can be read locally and point
// at the command that asks the server.
func TestAuthStatusWithAnOpaqueToken(t *testing.T) {
	isolateHome(t)
	loginWithToken(t, "sekret")

	_, err := execute(t, "auth", "status")
	if err == nil {
		t.Fatal("auth status on an opaque token should fail")
	}
	if !strings.Contains(err.Error(), "not a JWT") {
		t.Errorf("error should say the token is not a JWT: %v", err)
	}
	if !strings.Contains(err.Error(), "rossoctl status") {
		t.Errorf("error should name `rossoctl status` as the alternative: %v", err)
	}
}

// TestAuthStatusSendsNothingToTheServer is the behavior the help text promises:
// the report must work against a server that is unreachable.
func TestAuthStatusSendsNothingToTheServer(t *testing.T) {
	isolateHome(t)
	// Port 0 on localhost is never listening, so any request would fail.
	if _, err := execute(t, "config", "create-context",
		"--name", "dead", "--server", "http://127.0.0.1:0/api/v1/",
		"--bearer-token", testToken(t, map[string]any{"preferred_username": "ada"})); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	out, err := execute(t, "auth", "status")
	if err != nil {
		t.Fatalf("auth status must not contact the server: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ada") {
		t.Errorf("output missing the username:\n%s", out)
	}
}

// TestAuthStatusHonorsContextOverride confirms --context selects which stored
// token is inspected.
func TestAuthStatusHonorsContextOverride(t *testing.T) {
	isolateHome(t)
	loginWithToken(t, testToken(t, map[string]any{"preferred_username": "current-user"}))
	if _, err := execute(t, "config", "create-context",
		"--name", "other", "--server", "http://other/api/v1/",
		"--bearer-token", testToken(t, map[string]any{"preferred_username": "other-user"})); err != nil {
		t.Fatalf("create-context other: %v", err)
	}
	// create-context makes the new context current, so name dev explicitly.
	out, err := execute(t, "auth", "status", "--context", "dev")
	if err != nil {
		t.Fatalf("auth status --context dev: %v\n%s", err, out)
	}
	if !strings.Contains(out, "current-user") {
		t.Errorf("--context dev should report dev's token:\n%s", out)
	}
	if strings.Contains(out, "other-user") {
		t.Errorf("--context dev must not report another context's token:\n%s", out)
	}
}

// TestPrintAuthStatusExpiryLabel exercises the relative-time rendering directly,
// which is awkward to pin through the command because it depends on the clock.
func TestPrintAuthStatusExpiryLabel(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ctx := &config.Context{Name: "dev"}

	var buf bytes.Buffer
	printAuthStatus(&buf, ctx, &jwt.Claims{ExpiresAt: now.Add(90 * time.Minute).Unix()}, now)
	if got := buf.String(); !strings.Contains(got, "in 1h30m0s") {
		t.Errorf("a future expiry should show the remaining time:\n%s", got)
	}

	buf.Reset()
	printAuthStatus(&buf, ctx, &jwt.Claims{ExpiresAt: now.Add(-2 * time.Minute).Unix()}, now)
	if got := buf.String(); !strings.Contains(got, "2m0s ago") {
		t.Errorf("a past expiry should show how long ago:\n%s", got)
	}
}
