package jwt

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// makeToken builds a JWS-shaped token whose payload is claims. The header and
// signature are placeholders: Decode ignores both, and building a real one would
// test the signing library rather than this package.
func makeToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc(payload) + ".c2ln"
}

func TestDecodeReportsEveryClaimTheCommandShows(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	token := makeToken(t, map[string]any{
		"iss":                "https://keycloak.localtest.me/realms/rossoctl",
		"sub":                "user-uuid",
		"name":               "Ada Lovelace",
		"preferred_username": "ada",
		"email":              "ada@example.com",
		"aud":                []string{"rossoctl-ui", "account"},
		"exp":                exp,
		"iat":                time.Now().Unix(),
		"scope":              "openid profile email",
		"realm_access":       map[string]any{"roles": []string{"admin", "user"}},
	})

	c, err := Decode(token)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if c.Issuer != "https://keycloak.localtest.me/realms/rossoctl" {
		t.Errorf("Issuer = %q", c.Issuer)
	}
	if c.Name != "Ada Lovelace" || c.Username != "ada" || c.Email != "ada@example.com" {
		t.Errorf("identity claims = %q / %q / %q", c.Name, c.Username, c.Email)
	}
	if c.Subject != "user-uuid" {
		t.Errorf("Subject = %q", c.Subject)
	}
	if got := strings.Join(c.Audience, ","); got != "rossoctl-ui,account" {
		t.Errorf("Audience = %q", got)
	}
	if got := strings.Join(c.Scopes(), ","); got != "openid,profile,email" {
		t.Errorf("Scopes() = %q", got)
	}
	if got := strings.Join(c.Roles(), ","); got != "admin,user" {
		t.Errorf("Roles() = %q", got)
	}
	if c.ExpiresAt != exp {
		t.Errorf("ExpiresAt = %d, want %d", c.ExpiresAt, exp)
	}
}

// TestDecodeAudienceAcceptsAStringOrAnArray covers RFC 7519 §4.1.3, which
// permits both forms. Keycloak emits the bare string when there is one audience,
// so the single-string case is the common one in practice.
func TestDecodeAudienceAcceptsAStringOrAnArray(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		want    string
	}{
		{"single string", `{"aud":"rossoctl-ui"}`, "rossoctl-ui"},
		{"array", `{"aud":["a","b"]}`, "a,b"},
		{"absent", `{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			enc := base64.RawURLEncoding.EncodeToString
			token := enc([]byte(`{}`)) + "." + enc([]byte(tc.payload)) + ".sig"
			c, err := Decode(token)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got := strings.Join(c.Audience, ","); got != tc.want {
				t.Errorf("Audience = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeRejectsNonJWTs(t *testing.T) {
	for _, tc := range []struct{ name, token, wantSubstr string }{
		{"empty", "", "empty"},
		{"opaque", "sekret", "not a JWT"},
		{"two segments", "aaa.bbb", "not a JWT"},
		{"bad base64", "aaa.!!!!.ccc", "decoding the token payload"},
		{"payload not JSON", "aaa." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".ccc", "as JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.token)
			if err == nil {
				t.Fatalf("Decode(%q) succeeded, want an error", tc.token)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantSubstr)
			}
		})
	}
}

// TestDecodeToleratesPaddedBase64 covers a token produced by an encoder that
// pads, which is not what RFC 7515 specifies but is cheap to accept.
func TestDecodeToleratesPaddedBase64(t *testing.T) {
	payload := base64.URLEncoding.EncodeToString([]byte(`{"sub":"abc"}`))
	if !strings.HasSuffix(payload, "=") {
		t.Fatalf("test needs a padded payload; got %q", payload)
	}
	c, err := Decode("aaa." + payload + ".ccc")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if c.Subject != "abc" {
		t.Errorf("Subject = %q, want abc", c.Subject)
	}
}

func TestExpiryAndExpired(t *testing.T) {
	now := time.Unix(1_000_000, 0)

	// A token with no exp claim does not expire, which Expiry must distinguish
	// from one that expired at the epoch.
	var none Claims
	if _, ok := none.Expiry(); ok {
		t.Error("Expiry() reported a time for a token with no exp claim")
	}
	if none.Expired(now) {
		t.Error("a token with no exp claim must not be reported as expired")
	}

	past := Claims{ExpiresAt: now.Add(-time.Second).Unix()}
	if !past.Expired(now) {
		t.Error("a token whose exp has passed must be expired")
	}

	future := Claims{ExpiresAt: now.Add(time.Second).Unix()}
	if future.Expired(now) {
		t.Error("a token whose exp is in the future must not be expired")
	}

	// Exactly at exp counts as expired: the server's check is exp > now.
	atExp := Claims{ExpiresAt: now.Unix()}
	if !atExp.Expired(now) {
		t.Error("a token exactly at its exp must be expired")
	}
}

func TestScopesAndRolesAreEmptyWhenAbsent(t *testing.T) {
	var c Claims
	if got := c.Scopes(); len(got) != 0 {
		t.Errorf("Scopes() = %v, want empty", got)
	}
	if got := c.Roles(); len(got) != 0 {
		t.Errorf("Roles() = %v, want empty", got)
	}
	// A blank scope string must not yield one empty scope.
	c.Scope = "   "
	if got := c.Scopes(); len(got) != 0 {
		t.Errorf("Scopes() on a blank claim = %v, want empty", got)
	}
}
