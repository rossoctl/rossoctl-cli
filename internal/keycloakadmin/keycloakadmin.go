// Package keycloakadmin creates and removes the Keycloak objects the Rosso
// operator creates when it registers a workload for OIDC.
//
// It mirrors the operator's keycloak.Admin.RegisterOrFetchClient and
// EnsureAudienceScope (github.com/rossoctl/operator/internal/keycloak), and
// Unregister is their inverse. Those two calls create more than an OAuth client,
// and the parts differ in how long they outlive the workload:
//
//   - the OAuth client, whose clientId is the workload's SPIFFE ID;
//   - a client scope named agent-<namespace>-<workload>-aud, carrying an
//     oidc-audience-mapper naming the client as a custom audience;
//   - a realm-level default-default-client-scope entry for that scope;
//   - a default-client-scope attachment on each platform client (the UI).
//
// The last two are the reason this package exists rather than the work being a
// single DELETE. They are realm-global and live on long-lived shared clients, so
// nothing removes them when a workload goes away: Kubernetes garbage collection
// cannot see them, and the operator installs no finalizer that would. Left
// behind, the realm entry makes every client created afterwards inherit a
// retired workload's audience scope.
//
// Deleting a client scope in Keycloak cascades to its protocol mappers and to
// the scope's attachments, so the explicit unlinking here is belt-and-braces for
// the shared objects. It is done first regardless, because a link left pointing
// at a deleted scope is the failure this is meant to prevent.
package keycloakadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds a single admin request. Keycloak is a local service in
// the development setup this targets, so the budget is for a server that
// accepted the connection and then stalled rather than for a slow network.
const DefaultTimeout = 30 * time.Second

// Client talks to the Keycloak admin REST API.
//
// BaseURL is the server root with no trailing path, e.g.
// http://keycloak.localtest.me:8080.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client

	// Logf, when non-nil, receives one line per object removed or skipped. The
	// command layer points this at its --verbose writer; the deletion logic
	// itself does not decide what a user sees.
	Logf func(format string, args ...any)
}

func (c *Client) httpc() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: DefaultTimeout}
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// base is the server root without a trailing slash, matching the operator's
// trimBaseURL so a URL supplied either way produces the same endpoints.
func (c *Client) base() string {
	return strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
}

// SpiffeClientID is the OAuth clientId the operator registers for a workload.
//
// The workload name is deliberately absent: the operator's
// resolveKeycloakClientID keys the client on the ServiceAccount, so every
// workload in a namespace running under one ServiceAccount shares a single
// Keycloak client. That is why deleting a client is not a per-workload
// operation, and why UnregisterOptions.DeleteClient is opt-in.
func SpiffeClientID(trustDomain, namespace, serviceAccount string) string {
	return fmt.Sprintf("spiffe://%s/ns/%s/sa/%s", trustDomain, namespace, serviceAccount)
}

// AudienceScopeName derives the client-scope name from the operator's
// CLIENT_NAME, which is "<namespace>/<workload>".
//
// This mirrors keycloak.AudienceScopeName exactly, including that the
// slash-to-dash replacement is lossy: "a/b-c" and "a-b/c" both yield
// "agent-a-b-c-aud". Reproducing the lossy form is correct here — the name being
// matched is the one the operator actually created — but it means a caller can
// name a scope belonging to a different workload, so the deletion is reported
// rather than silent.
func AudienceScopeName(namespace, workload string) string {
	return "agent-" + strings.ReplaceAll(namespace+"/"+workload, "/", "-") + "-aud"
}

// ValidateRealm rejects a realm name that would not survive being placed in a URL
// path as a single segment.
//
// url.PathEscape escapes a slash but leaves dot segments alone, so a realm of
// "../master" produces /admin/realms/../master/clients — a path that this code
// never resolves but that a server or proxy may normalize, retargeting the request
// at a realm the caller did not name. Since the realm reaches here from a flag and
// every request this package makes is a DELETE, it is rejected rather than
// sanitized: a name that needs rewriting to be safe is not the name of the realm
// the caller meant.
func ValidateRealm(realm string) error {
	switch {
	case strings.TrimSpace(realm) == "":
		return fmt.Errorf("realm must not be empty")
	case realm != strings.TrimSpace(realm):
		return fmt.Errorf("realm %q has leading or trailing whitespace", realm)
	case strings.ContainsAny(realm, "/\\"):
		return fmt.Errorf("realm %q must not contain a path separator", realm)
	case realm == "." || realm == "..":
		return fmt.Errorf("realm %q is not a realm name", realm)
	}
	return nil
}

// UnregisterOptions describes one workload's registration to remove.
type UnregisterOptions struct {
	Realm     string
	ClientID  string   // the OAuth clientId, i.e. the workload's SPIFFE ID
	ScopeName string   // the audience client scope, e.g. agent-ns1-agent-a-aud
	Platforms []string // platform clientIds to unlink the scope from

	// DeleteClient removes the OAuth client itself. Off by default: the client is
	// per-ServiceAccount, not per-workload, so removing it can break sibling
	// workloads that share the ServiceAccount.
	DeleteClient bool
}

// DefaultPlatformClientIDs is what the operator's parsePlatformClientIDs falls
// back to when PLATFORM_CLIENT_IDS is unset, so it is what a default
// installation attached the scope to.
var DefaultPlatformClientIDs = []string{"rossoctl"}

// Result records what Unregister did, so the caller can report an accurate
// summary rather than assuming every step applied.
type Result struct {
	ScopeDeleted     bool
	ScopeAbsent      bool // no such scope: already unregistered, or a name typo
	RealmLinkRemoved bool
	PlatformUnlinked []string
	ClientDeleted    bool
	ClientAbsent     bool
	// ClientSkipped is set when a client exists but DeleteClient was not
	// requested, which is the default and not an error.
	ClientSkipped bool
	ClientID      string
}

// Unregister removes the objects the operator's registration created.
//
// Order matters. The scope's attachments are removed before the scope itself, so
// an interrupted run never leaves a shared client or the realm pointing at a
// scope that no longer exists; the client is deleted last, since it is the one
// step with consequences beyond this workload.
//
// A missing object is not an error. This is cleanup, and the useful behaviour
// for a command a user may run twice, or after a partial failure, is to
// converge: what is absent is reported as absent and the remaining steps still
// run.
func (c *Client) Unregister(ctx context.Context, token string, o UnregisterOptions) (Result, error) {
	var res Result
	res.ClientID = o.ClientID

	// Checked here rather than only in the command layer: every request below is a
	// DELETE, so a realm that retargets the path must not get as far as being sent.
	if err := ValidateRealm(o.Realm); err != nil {
		return res, err
	}

	scopeID, err := c.findClientScopeIDByName(ctx, token, o.Realm, o.ScopeName)
	if err != nil {
		return res, err
	}

	if scopeID == "" {
		res.ScopeAbsent = true
		c.logf("client scope %q not found; nothing to unlink", o.ScopeName)
	} else {
		// Unlink before deleting. Keycloak cascades these when the scope goes, but
		// a dangling link is exactly the leak this command exists to clean up, so
		// it is not left to depend on that.
		if err := c.deleteRealmDefaultDefaultClientScope(ctx, token, o.Realm, scopeID); err != nil {
			return res, err
		}
		res.RealmLinkRemoved = true
		c.logf("removed realm default-default-client-scope link for %q", o.ScopeName)

		for _, plat := range o.Platforms {
			plat = strings.TrimSpace(plat)
			if plat == "" {
				continue
			}
			// A platform client that does not exist is skipped rather than failing,
			// as the operator's own attachment loop does: the deployment may simply
			// not include that client.
			internalID, err := c.findClientUUID(ctx, token, o.Realm, plat)
			if err != nil {
				return res, err
			}
			if internalID == "" {
				c.logf("platform client %q not found; skipping", plat)
				continue
			}
			if err := c.deleteClientDefaultClientScope(ctx, token, o.Realm, internalID, scopeID); err != nil {
				return res, err
			}
			res.PlatformUnlinked = append(res.PlatformUnlinked, plat)
			c.logf("removed scope link on platform client %q", plat)
		}

		// Deleting the scope cascades to its oidc-audience-mapper.
		if err := c.deleteClientScope(ctx, token, o.Realm, scopeID); err != nil {
			return res, err
		}
		res.ScopeDeleted = true
		c.logf("deleted client scope %q", o.ScopeName)
	}

	internalID, err := c.findClientUUID(ctx, token, o.Realm, o.ClientID)
	if err != nil {
		return res, err
	}
	switch {
	case internalID == "":
		res.ClientAbsent = true
		c.logf("client %q not found", o.ClientID)
	case !o.DeleteClient:
		res.ClientSkipped = true
		c.logf("client %q left in place", o.ClientID)
	default:
		// This also removes the service-account-<clientId> user Keycloak created
		// implicitly for serviceAccountsEnabled, and the client's own scope
		// attachments.
		if err := c.deleteClient(ctx, token, o.Realm, internalID); err != nil {
			return res, err
		}
		res.ClientDeleted = true
		c.logf("deleted client %q", o.ClientID)
	}
	return res, nil
}

// PasswordGrantToken obtains an admin token from the master realm using the
// admin-cli public client, the same grant the operator's Admin uses.
//
// The master realm is not configurable: it is where Keycloak keeps the admin
// account, independent of the realm being modified.
func (c *Client) PasswordGrantToken(ctx context.Context, adminUser, adminPass string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("client_id", "admin-cli")
	form.Set("username", adminUser)
	form.Set("password", adminPass)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base()+"/realms/master/protocol/openid-connect/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpc().Do(req)
	if err != nil {
		return "", fmt.Errorf("keycloak token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != http.StatusOK {
		// The password is in the request, not the response, so the body is safe to
		// quote and is usually the only clue: Keycloak answers 401 both for a wrong
		// password and for an account without admin rights.
		return "", fmt.Errorf("keycloak token: status %d: %s", resp.StatusCode, truncate(body, 512))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("keycloak token decode: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("keycloak token: response carried no access_token")
	}
	return tr.AccessToken, nil
}

// maxBody caps an admin response read. Admin replies are small; the cap is so a
// misdirected URL answering with something huge cannot exhaust memory here.
const maxBody = 1 << 20

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// do performs an authenticated admin request and returns the status and body.
func (c *Client) do(ctx context.Context, token, method, endpoint string, payload []byte) (int, []byte, error) {
	status, body, _, err := c.doWithHeader(ctx, token, method, endpoint, payload)
	return status, body, err
}

// doWithHeader is do, additionally returning the response headers. Keycloak
// reports the id of an object it just created in Location rather than in the
// body, so the creation calls need them.
func (c *Client) doWithHeader(ctx context.Context, token, method, endpoint string, payload []byte) (int, []byte, http.Header, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpc().Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, nil, resp.Header, err
	}
	return resp.StatusCode, body, resp.Header, nil
}

// findClientUUID resolves a clientId to Keycloak's internal UUID, returning an
// empty string when no such client exists.
//
// The query parameter is a filter rather than an exact match, so the result is
// compared: Keycloak's clientId search has matched on substrings in some
// versions, and deleting a client whose id merely contains the one asked for
// would be the worst possible outcome here.
func (c *Client) findClientUUID(ctx context.Context, token, realm, clientID string) (string, error) {
	u, err := url.Parse(c.base() + "/admin/realms/" + url.PathEscape(realm) + "/clients")
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("clientId", clientID)
	u.RawQuery = q.Encode()

	status, body, err := c.do(ctx, token, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("keycloak list clients: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("keycloak list clients: status %d: %s", status, truncate(body, 512))
	}
	var list []struct {
		ID       string `json:"id"`
		ClientID string `json:"clientId"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("keycloak list clients decode: %w", err)
	}
	for i := range list {
		if list[i].ClientID == clientID {
			return list[i].ID, nil
		}
	}
	return "", nil
}

// findClientScopeIDByName resolves a client-scope name to its id, returning an
// empty string when no scope has that name.
func (c *Client) findClientScopeIDByName(ctx context.Context, token, realm, name string) (string, error) {
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes"
	status, body, err := c.do(ctx, token, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("keycloak list client-scopes: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("keycloak list client-scopes: status %d: %s", status, truncate(body, 512))
	}
	var list []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("keycloak list client-scopes decode: %w", err)
	}
	for i := range list {
		if list[i].Name == name {
			return list[i].ID, nil
		}
	}
	return "", nil
}

// deleteExpectSuccess issues a DELETE, treating 404 as success.
//
// A 404 means the object is already gone, which is the state the caller wanted.
// Reporting it as an error would make a second run of this command fail after a
// successful first one, and would turn a partially-completed cleanup into
// something a user cannot finish by re-running.
func (c *Client) deleteExpectSuccess(ctx context.Context, token, endpoint, what string) error {
	status, body, err := c.do(ctx, token, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("keycloak delete %s: %w", what, err)
	}
	switch status {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	}
	return fmt.Errorf("keycloak delete %s: status %d: %s", what, status, truncate(body, 512))
}

func (c *Client) deleteClient(ctx context.Context, token, realm, internalUUID string) error {
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) + "/clients/" + url.PathEscape(internalUUID)
	return c.deleteExpectSuccess(ctx, token, endpoint, "client")
}

func (c *Client) deleteClientScope(ctx context.Context, token, realm, scopeID string) error {
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) + "/client-scopes/" + url.PathEscape(scopeID)
	return c.deleteExpectSuccess(ctx, token, endpoint, "client-scope")
}

// deleteRealmDefaultDefaultClientScope is the inverse of the operator's
// putRealmDefaultDefaultClientScope. Removing this entry stops clients created
// later in the realm from inheriting the scope.
func (c *Client) deleteRealmDefaultDefaultClientScope(ctx context.Context, token, realm, scopeID string) error {
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) +
		"/default-default-client-scopes/" + url.PathEscape(scopeID)
	return c.deleteExpectSuccess(ctx, token, endpoint, "realm default-default-client-scope")
}

// deleteClientDefaultClientScope is the inverse of the operator's
// putClientDefaultClientScope.
func (c *Client) deleteClientDefaultClientScope(ctx context.Context, token, realm, clientUUID, scopeID string) error {
	endpoint := c.base() + "/admin/realms/" + url.PathEscape(realm) +
		"/clients/" + url.PathEscape(clientUUID) +
		"/default-client-scopes/" + url.PathEscape(scopeID)
	return c.deleteExpectSuccess(ctx, token, endpoint, "client default-client-scope")
}
