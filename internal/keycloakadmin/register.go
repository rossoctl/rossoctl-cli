package keycloakadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Client authenticator types, matching the operator's CLIENT_AUTH_TYPE values.
const (
	// AuthTypeFederatedJWT authenticates the client by a JWT it signs itself,
	// which for a Rosso workload is the SPIFFE JWT-SVID from its SPIRE agent.
	// There is no client secret in this mode.
	AuthTypeFederatedJWT = "federated-jwt"

	// AuthTypeClientSecret authenticates the client by a shared secret Keycloak
	// generates. This is the operator's fallback when CLIENT_AUTH_TYPE is unset.
	AuthTypeClientSecret = "client-secret"
)

// DefaultSpiffeIDPAlias is the operator's fallback when SPIFFE_IDP_ALIAS is
// unset: the alias of the Keycloak identity provider that trusts SPIRE.
const DefaultSpiffeIDPAlias = "spire-spiffe"

// oidcAudienceMapper is Keycloak's built-in mapper that adds a fixed string to
// the access token's aud claim.
const oidcAudienceMapper = "oidc-audience-mapper"

// RegisterOptions describes one workload's registration to create.
type RegisterOptions struct {
	Realm      string
	ClientID   string // the OAuth clientId, i.e. the workload's SPIFFE ID
	ClientName string // Keycloak's human-readable name field, "<namespace>/<workload>"
	ScopeName  string // the audience client scope, e.g. agent-ns1-agent-a-aud

	// Audience is the value the scope's mapper writes into the aud claim. The
	// operator passes the client's own clientId, so a token issued to the workload
	// names the workload as its own audience — which is what lets the workload's
	// inbound JWT validation accept it.
	Audience string

	// AuthType is AuthTypeFederatedJWT or AuthTypeClientSecret. Empty means
	// AuthTypeClientSecret, matching the operator's fallback.
	AuthType string

	// SpiffeIDPAlias names the Keycloak identity provider trusted to have issued
	// the client's JWT. Only used for AuthTypeFederatedJWT; empty means
	// DefaultSpiffeIDPAlias.
	SpiffeIDPAlias string

	// TokenExchangeEnable sets the standard.token.exchange.enabled attribute,
	// which a workload needs in order to exchange its token for a downstream one.
	TokenExchangeEnable bool

	// Platforms are platform clientIds to attach the audience scope to, so tokens
	// those clients obtain also carry this workload's audience.
	Platforms []string
}

// RegisterResult records what Register did, so the caller can distinguish a
// created object from a reused one rather than reporting both as success.
type RegisterResult struct {
	ClientID       string
	ClientCreated  bool
	ClientExisted  bool
	ClientUUID     string
	ClientSecret   string // empty for federated-jwt, which has no secret
	AuthType       string // the type actually requested, after defaulting
	ScopeCreated   bool
	ScopeExisted   bool
	MapperCreated  bool
	RealmLinked    bool
	ClientLinked   bool
	PlatformLinked []string

	// Drift lists ways an already-existing client differs from what Register
	// would have created. Register never changes an existing client, so this is
	// reported for the caller to act on: a client whose authenticator type does
	// not match will not authenticate the workload.
	Drift []string
}

// Register creates the objects the operator's registration creates.
//
// It is safe to run against a realm that is already registered. An existing
// client is reused and left untouched, an existing scope is reused, and the
// attachments are PUTs that Keycloak treats as idempotent — so a partially
// completed registration is finished by running this again.
//
// Order mirrors the operator's: the client first, because the audience scope is
// attached to it and Keycloak does not retroactively apply realm default-default
// scopes to clients that already exist when the scope appears.
func (c *Client) Register(ctx context.Context, token string, o RegisterOptions) (RegisterResult, error) {
	var res RegisterResult
	res.ClientID = o.ClientID

	// Checked here rather than only in the command layer, so a direct caller of
	// this package cannot send a realm that retargets the URL path.
	if err := ValidateRealm(o.Realm); err != nil {
		return res, err
	}

	authType := o.AuthType
	if authType == "" {
		authType = AuthTypeClientSecret
	}
	if authType != AuthTypeFederatedJWT && authType != AuthTypeClientSecret {
		return res, fmt.Errorf("unsupported client auth type %q: want %q or %q",
			authType, AuthTypeFederatedJWT, AuthTypeClientSecret)
	}
	res.AuthType = authType

	rep := desiredClientRep(o, authType)

	internalID, err := c.findClientUUID(ctx, token, o.Realm, o.ClientID)
	if err != nil {
		return res, err
	}
	if internalID == "" {
		internalID, err = c.createClient(ctx, token, o.Realm, rep)
		if err != nil {
			return res, err
		}
		res.ClientCreated = true
		c.logf("created client %q", o.ClientID)
	} else {
		res.ClientExisted = true
		c.logf("client %q already exists; reused", o.ClientID)

		// Reused rather than reconciled, so a client shared by sibling workloads
		// is never mutated by registering one of them. Drift is reported instead:
		// a mismatch here is the difference between a workload that authenticates
		// and one that does not, so it must not be silent.
		drift, err := c.clientDrift(ctx, token, o.Realm, internalID, rep)
		if err != nil {
			return res, err
		}
		res.Drift = drift
		for _, d := range drift {
			c.logf("existing client differs: %s", d)
		}
	}
	res.ClientUUID = internalID

	// Only client-secret clients have a secret to read. Asking for one in
	// federated-jwt mode would return Keycloak's unused placeholder, which would
	// be worse than nothing: it looks like a usable credential.
	if authType == AuthTypeClientSecret {
		secret, err := c.readClientSecret(ctx, token, o.Realm, internalID)
		if err != nil {
			return res, err
		}
		res.ClientSecret = secret
	}

	scopeID, err := c.findClientScopeIDByName(ctx, token, o.Realm, o.ScopeName)
	if err != nil {
		return res, err
	}
	if scopeID == "" {
		scopeID, err = c.createClientScope(ctx, token, o.Realm, o.ScopeName)
		if err != nil {
			return res, err
		}
		res.ScopeCreated = true
		c.logf("created client scope %q", o.ScopeName)

		// The mapper is what makes the scope do anything; a scope without it is
		// an empty shell. It is created only with the scope, as the operator does
		// — an existing scope's mappers are left alone.
		if err := c.createAudienceMapper(ctx, token, o.Realm, scopeID, o.ScopeName, o.Audience); err != nil {
			return res, err
		}
		res.MapperCreated = true
		c.logf("added audience mapper for %q", o.Audience)
	} else {
		res.ScopeExisted = true
		c.logf("client scope %q already exists; reused", o.ScopeName)
	}

	// Realm-level, so clients created after this inherit the scope.
	if err := c.putRealmDefaultDefaultClientScope(ctx, token, o.Realm, scopeID); err != nil {
		return res, err
	}
	res.RealmLinked = true
	c.logf("linked scope %q as a realm default", o.ScopeName)

	// The realm default above does not apply to this client if it already
	// existed, so it is attached explicitly.
	if err := c.putClientDefaultClientScope(ctx, token, o.Realm, internalID, scopeID); err != nil {
		return res, err
	}
	res.ClientLinked = true
	c.logf("attached scope %q to client %q", o.ScopeName, o.ClientID)

	for _, plat := range o.Platforms {
		plat = strings.TrimSpace(plat)
		if plat == "" {
			continue
		}
		// A platform client that is not deployed is skipped rather than failing,
		// matching the operator's own attachment loop.
		platUUID, err := c.findClientUUID(ctx, token, o.Realm, plat)
		if err != nil {
			return res, err
		}
		if platUUID == "" {
			c.logf("platform client %q not found; skipping", plat)
			continue
		}
		if err := c.putClientDefaultClientScope(ctx, token, o.Realm, platUUID, scopeID); err != nil {
			return res, err
		}
		res.PlatformLinked = append(res.PlatformLinked, plat)
		c.logf("attached scope %q to platform client %q", o.ScopeName, plat)
	}

	return res, nil
}

// clientRep is the Keycloak client representation, holding the fields the
// operator manages. Field names and the set of them mirror the operator's
// keycloakClientRep, since the object this creates has to be the one the
// operator would have created.
type clientRep struct {
	ClientID string `json:"clientId"`
	Name     string `json:"name"`

	StandardFlowEnabled       bool              `json:"standardFlowEnabled"`
	DirectAccessGrantsEnabled bool              `json:"directAccessGrantsEnabled"`
	ServiceAccountsEnabled    bool              `json:"serviceAccountsEnabled"`
	FullScopeAllowed          bool              `json:"fullScopeAllowed"`
	PublicClient              bool              `json:"publicClient"`
	ClientAuthenticatorType   string            `json:"clientAuthenticatorType"`
	Attributes                map[string]string `json:"attributes"`
}

// desiredClientRep builds the representation the operator would send.
//
// ServiceAccountsEnabled is what gives the client the client_credentials grant it
// uses to get its own token. FullScopeAllowed is false so the client gets only
// the scopes explicitly attached to it, which is what makes the audience scope
// attachment meaningful.
func desiredClientRep(o RegisterOptions, authType string) *clientRep {
	attrs := map[string]string{
		"standard.token.exchange.enabled": fmt.Sprintf("%t", o.TokenExchangeEnable),
	}
	if authType == AuthTypeFederatedJWT {
		alias := strings.TrimSpace(o.SpiffeIDPAlias)
		if alias == "" {
			alias = DefaultSpiffeIDPAlias
		}
		// The issuer Keycloak requires the client's JWT to come from, and the
		// subject it must carry — the workload's SPIFFE ID.
		attrs["jwt.credential.issuer"] = alias
		attrs["jwt.credential.sub"] = o.ClientID
	}
	return &clientRep{
		ClientID:                  o.ClientID,
		Name:                      o.ClientName,
		StandardFlowEnabled:       true,
		DirectAccessGrantsEnabled: true,
		ServiceAccountsEnabled:    true,
		FullScopeAllowed:          false,
		PublicClient:              false,
		ClientAuthenticatorType:   authType,
		Attributes:                attrs,
	}
}

// createClient POSTs a new client and returns its internal UUID.
//
// A 409 means another actor created it between the lookup and here, which is the
// state the caller wanted, so the id is fetched rather than the race reported.
func (c *Client) createClient(ctx context.Context, token, realm string, rep *clientRep) (string, error) {
	payload, err := json.Marshal(rep)
	if err != nil {
		return "", err
	}
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) + "/clients"
	status, body, hdr, err := c.doWithHeader(ctx, token, http.MethodPost, endpoint, payload)
	if err != nil {
		return "", fmt.Errorf("keycloak create client: %w", err)
	}
	switch status {
	case http.StatusCreated:
		// Keycloak returns the new object's URL rather than its body.
		if id := pathLastSegment(hdr.Get("Location")); id != "" {
			return id, nil
		}
		return c.requireClientUUID(ctx, token, realm, rep.ClientID)
	case http.StatusConflict:
		return c.requireClientUUID(ctx, token, realm, rep.ClientID)
	}
	return "", fmt.Errorf("keycloak create client: status %d: %s", status, truncate(body, 512))
}

// requireClientUUID looks up a client that is expected to exist, turning an
// absence into an error rather than an empty id that a caller might use.
func (c *Client) requireClientUUID(ctx context.Context, token, realm, clientID string) (string, error) {
	id, err := c.findClientUUID(ctx, token, realm, clientID)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("keycloak created client %q but it cannot be found", clientID)
	}
	return id, nil
}

// createClientScope POSTs a new client scope and returns its id.
//
// The attributes match the operator's: include.in.token.scope puts the scope in
// the token's scope claim, and display.on.consent.screen affects only the consent
// UI, which these machine clients never show.
func (c *Client) createClientScope(ctx context.Context, token, realm, name string) (string, error) {
	rep := struct {
		Name       string            `json:"name"`
		Protocol   string            `json:"protocol"`
		Attributes map[string]string `json:"attributes,omitempty"`
	}{
		Name:     name,
		Protocol: "openid-connect",
		Attributes: map[string]string{
			"include.in.token.scope":    "true",
			"display.on.consent.screen": "true",
		},
	}
	payload, err := json.Marshal(rep)
	if err != nil {
		return "", err
	}
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes"
	status, body, hdr, err := c.doWithHeader(ctx, token, http.MethodPost, endpoint, payload)
	if err != nil {
		return "", fmt.Errorf("keycloak create client-scope: %w", err)
	}
	switch status {
	case http.StatusCreated:
		if id := pathLastSegment(hdr.Get("Location")); id != "" {
			return id, nil
		}
		return c.requireClientScopeID(ctx, token, realm, name)
	case http.StatusConflict:
		return c.requireClientScopeID(ctx, token, realm, name)
	}
	return "", fmt.Errorf("keycloak create client-scope: status %d: %s", status, truncate(body, 512))
}

func (c *Client) requireClientScopeID(ctx context.Context, token, realm, name string) (string, error) {
	id, err := c.findClientScopeIDByName(ctx, token, realm, name)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("keycloak created client scope %q but it cannot be found", name)
	}
	return id, nil
}

// createAudienceMapper adds the oidc-audience-mapper that puts Audience into the
// access token's aud claim.
//
// The claim flags mirror the operator's: the audience goes in the access token
// only, since it is the access token a workload presents to another workload.
func (c *Client) createAudienceMapper(ctx context.Context, token, realm, scopeID, scopeName, audience string) error {
	rep := struct {
		Name            string            `json:"name"`
		Protocol        string            `json:"protocol"`
		ProtocolMapper  string            `json:"protocolMapper"`
		ConsentRequired bool              `json:"consentRequired"`
		Config          map[string]string `json:"config"`
	}{
		Name:            scopeName,
		Protocol:        "openid-connect",
		ProtocolMapper:  oidcAudienceMapper,
		ConsentRequired: false,
		Config: map[string]string{
			"included.custom.audience": audience,
			"id.token.claim":           "false",
			"access.token.claim":       "true",
			"userinfo.token.claim":     "false",
		},
	}
	payload, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) +
		"/client-scopes/" + url.PathEscape(scopeID) + "/protocol-mappers/models"
	status, body, err := c.do(ctx, token, http.MethodPost, endpoint, payload)
	if err != nil {
		return fmt.Errorf("keycloak add audience mapper: %w", err)
	}
	// 409 means a mapper of this name is already on the scope, which is the state
	// wanted. Its audience is not checked or corrected here, matching the
	// operator's rule that an existing scope's mappers are not touched.
	if status == http.StatusCreated || status == http.StatusConflict ||
		(status >= 200 && status < 300) {
		return nil
	}
	return fmt.Errorf("keycloak add audience mapper: status %d: %s", status, truncate(body, 512))
}

// readClientSecret fetches the generated secret of a client-secret client.
func (c *Client) readClientSecret(ctx context.Context, token, realm, internalUUID string) (string, error) {
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) +
		"/clients/" + url.PathEscape(internalUUID) + "/client-secret"
	status, body, err := c.do(ctx, token, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("keycloak read client secret: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("keycloak read client secret: status %d: %s", status, truncate(body, 512))
	}
	var rep struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &rep); err != nil {
		return "", fmt.Errorf("keycloak read client secret decode: %w", err)
	}
	return rep.Value, nil
}

// clientDrift reports how an existing client differs from the desired one, for
// the fields that decide whether the workload can authenticate.
//
// Only those fields are compared. A realm's operator may legitimately have
// changed other settings, and listing every difference would bury the ones that
// break authentication.
func (c *Client) clientDrift(ctx context.Context, token, realm, internalUUID string, want *clientRep) ([]string, error) {
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) + "/clients/" + url.PathEscape(internalUUID)
	status, body, err := c.do(ctx, token, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("keycloak read client: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("keycloak read client: status %d: %s", status, truncate(body, 512))
	}
	var got struct {
		ClientAuthenticatorType string            `json:"clientAuthenticatorType"`
		ServiceAccountsEnabled  bool              `json:"serviceAccountsEnabled"`
		Attributes              map[string]string `json:"attributes"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		return nil, fmt.Errorf("keycloak read client decode: %w", err)
	}

	var drift []string
	if got.ClientAuthenticatorType != want.ClientAuthenticatorType {
		drift = append(drift, fmt.Sprintf(
			"clientAuthenticatorType is %q, this command would create %q",
			got.ClientAuthenticatorType, want.ClientAuthenticatorType))
	}
	if !got.ServiceAccountsEnabled {
		// Without this the client cannot use client_credentials, so it cannot get
		// a token at all.
		drift = append(drift, "serviceAccountsEnabled is false, so the client cannot obtain its own token")
	}
	// Only meaningful for federated-jwt, where the subject is what ties the
	// Keycloak client to the SPIFFE identity presenting the JWT.
	if sub, ok := want.Attributes["jwt.credential.sub"]; ok {
		if got.Attributes["jwt.credential.sub"] != sub {
			drift = append(drift, fmt.Sprintf(
				"jwt.credential.sub is %q, this command would set %q",
				got.Attributes["jwt.credential.sub"], sub))
		}
	}
	return drift, nil
}

// putRealmDefaultDefaultClientScope adds the scope to the realm's default
// defaults, so clients created afterwards inherit it. Inverse of
// deleteRealmDefaultDefaultClientScope.
func (c *Client) putRealmDefaultDefaultClientScope(ctx context.Context, token, realm, scopeID string) error {
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) +
		"/default-default-client-scopes/" + url.PathEscape(scopeID)
	return c.putExpectSuccess(ctx, token, endpoint, "realm default-default-client-scope")
}

// putClientDefaultClientScope attaches the scope to one client. Inverse of
// deleteClientDefaultClientScope.
func (c *Client) putClientDefaultClientScope(ctx context.Context, token, realm, clientUUID, scopeID string) error {
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) +
		"/clients/" + url.PathEscape(clientUUID) +
		"/default-client-scopes/" + url.PathEscape(scopeID)
	return c.putExpectSuccess(ctx, token, endpoint, "client default-client-scope")
}

// putExpectSuccess issues a body-less PUT, treating 409 as success.
//
// These attachment endpoints answer 409 when the link is already present, which
// is the state the caller wanted. Treating it as an error would make a re-run
// fail after a successful first one.
func (c *Client) putExpectSuccess(ctx context.Context, token, endpoint, what string) error {
	status, body, err := c.do(ctx, token, http.MethodPut, endpoint, nil)
	if err != nil {
		return fmt.Errorf("keycloak link %s: %w", what, err)
	}
	if status == http.StatusConflict || (status >= 200 && status < 300) {
		return nil
	}
	return fmt.Errorf("keycloak link %s: status %d: %s", what, status, truncate(body, 512))
}

// pathLastSegment returns the final segment of a Location URL, which is how
// Keycloak reports the id of an object it just created.
func pathLastSegment(loc string) string {
	loc = strings.TrimRight(loc, "/")
	if idx := strings.LastIndex(loc, "/"); idx >= 0 {
		return loc[idx+1:]
	}
	return ""
}
