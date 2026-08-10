// Package jwt decodes the claims of a JWT without verifying its signature.
//
// This is for inspecting a token rossoctl already holds — showing its subject,
// issuer, audiences, and expiry — not for deciding whether to trust one. The
// token in a context came from this machine's own login and is about to be sent
// to the server that will verify it; re-verifying it here would need the
// issuer's keys and would prove nothing the server does not already check.
//
// Nothing this package returns may be used as an authorization decision.
package jwt

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Claims holds the registered and Keycloak-specific claims rossoctl reports.
//
// Every field is optional: a token from a different issuer, or one issued with
// a narrower mapper set, simply leaves them zero. Callers distinguish "absent"
// from "empty" where it matters (see Expiry).
type Claims struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Name     string `json:"name"`
	Username string `json:"preferred_username"`
	Email    string `json:"email"`

	// Audience is the "aud" claim, which JWT allows to be either a single
	// string or an array of them. See its UnmarshalJSON.
	Audience Audience `json:"aud"`

	// ExpiresAt and IssuedAt are NumericDate claims: seconds since the Unix
	// epoch. Zero means the claim was absent.
	ExpiresAt int64 `json:"exp"`
	IssuedAt  int64 `json:"iat"`

	// Scope is the space-delimited OAuth scope string (RFC 8693 §4.2), not a
	// list. Scopes() splits it.
	Scope string `json:"scope"`

	// RealmAccess carries Keycloak's realm-level role list.
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// Audience is a JWT "aud" claim. RFC 7519 §4.1.3 allows either a single string
// or an array of strings, so this unmarshals both into a slice.
type Audience []string

// UnmarshalJSON accepts either a JSON string or an array of strings.
func (a *Audience) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*a = Audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("aud claim is neither a string nor an array of strings")
	}
	*a = Audience(many)
	return nil
}

// Expiry returns the expiration time and whether the token carried one. A
// token with no "exp" claim does not expire, which is a different thing from
// one that expired at the epoch — hence the second return value.
func (c *Claims) Expiry() (time.Time, bool) {
	if c.ExpiresAt == 0 {
		return time.Time{}, false
	}
	return time.Unix(c.ExpiresAt, 0), true
}

// Expired reports whether the token's expiry is at or before now. A token with
// no "exp" claim is never expired.
func (c *Claims) Expired(now time.Time) bool {
	exp, ok := c.Expiry()
	if !ok {
		return false
	}
	return !exp.After(now)
}

// Scopes splits the space-delimited "scope" claim into individual scopes, or
// returns nil when the claim is absent or blank.
func (c *Claims) Scopes() []string {
	return strings.Fields(c.Scope)
}

// Roles returns the realm_access.roles list.
func (c *Claims) Roles() []string {
	return c.RealmAccess.Roles
}

// Decode parses the claims from a JWS Compact Serialization token
// (header.payload.signature) without verifying the signature.
//
// Only the payload is decoded; the header and signature are ignored beyond
// checking that three segments are present. An opaque token — one that is not a
// JWT at all, which an OAuth server is free to issue — fails here, and the
// error says so rather than blaming the encoding.
func Decode(token string) (*Claims, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("token is empty")
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a JWT: expected 3 dot-separated segments, got %d", len(parts))
	}

	// JWT uses base64url without padding (RFC 7515 §2). RawURLEncoding is the
	// exact match; tolerate a padded encoder by trimming any "=" first.
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, fmt.Errorf("decoding the token payload: %w", err)
	}

	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parsing the token payload as JSON: %w", err)
	}
	return &claims, nil
}
