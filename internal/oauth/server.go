package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rousoftware/asgard/internal/auth"
	"github.com/rousoftware/asgard/internal/httpx"
	"github.com/rousoftware/asgard/internal/store"
)

const allScopes = "asgard:read asgard:operate asgard:deploy asgard:configure asgard:backup asgard:delete"

type Server struct {
	Store        *store.Store
	Auth         *auth.Service
	PublicURL    string
	Resource     string
	secureClient *http.Client
}
type Client struct {
	ID           string
	ClientID     string
	Name         string
	RedirectURIs []string
	GrantTypes   []string
}

func New(database *store.Store, authService *auth.Service, publicURL string) *Server {
	return &Server{Store: database, Auth: authService, PublicURL: publicURL, Resource: publicURL + "/mcp", secureClient: safeHTTPClient()}
}

func (s *Server) ProtectedResource(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{"resource": s.Resource, "authorization_servers": []string{s.PublicURL}, "bearer_methods_supported": []string{"header"}, "scopes_supported": strings.Fields(allScopes), "resource_name": "Asgard MCP"})
}
func (s *Server) AuthorizationMetadata(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{"issuer": s.PublicURL, "authorization_endpoint": s.PublicURL + "/oauth/authorize", "token_endpoint": s.PublicURL + "/oauth/token", "registration_endpoint": s.PublicURL + "/oauth/register", "revocation_endpoint": s.PublicURL + "/oauth/revoke", "jwks_uri": s.PublicURL + "/.well-known/jwks.json", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": strings.Fields(allScopes), "resource_parameter_supported": true, "client_id_metadata_document_supported": true})
}
func (s *Server) JWKS(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, s.Auth.Signer().JWKS())
}

func (s *Server) Register(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "Invalid client metadata.")
		return
	}
	body.ClientName = strings.TrimSpace(body.ClientName)
	if body.ClientName == "" || len(body.ClientName) > 100 || len(body.RedirectURIs) == 0 || len(body.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "Client name and one to ten redirect URIs are required.")
		return
	}
	for _, redirect := range body.RedirectURIs {
		if err := validateRedirectURI(redirect); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	if len(body.GrantTypes) == 0 {
		body.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	for _, grant := range body.GrantTypes {
		if grant != "authorization_code" && grant != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "Only authorization_code and refresh_token grants are supported.")
			return
		}
	}
	if body.TokenEndpointAuthMethod != "" && body.TokenEndpointAuthMethod != "none" {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "Only public clients with token_endpoint_auth_method none are supported.")
		return
	}
	var count int
	_ = s.Store.DB.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM oauth_clients`).Scan(&count)
	if count >= 500 {
		oauthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "Client registration limit reached.")
		return
	}
	clientID := "asgard-client-" + random(18)
	redirectJSON, _ := json.Marshal(unique(body.RedirectURIs))
	grantsJSON, _ := json.Marshal(unique(body.GrantTypes))
	_, err := s.Store.DB.ExecContext(r.Context(), `INSERT INTO oauth_clients(id,client_id,client_name,redirect_uris,grant_types,token_endpoint_auth_method,created_at) VALUES(?,?,?,?,?,'none',?)`, uuid.NewString(), clientID, body.ClientName, string(redirectJSON), string(grantsJSON), store.Now())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"client_id": clientID, "client_name": body.ClientName, "redirect_uris": unique(body.RedirectURIs), "grant_types": unique(body.GrantTypes), "response_types": []string{"code"}, "token_endpoint_auth_method": "none", "client_id_issued_at": time.Now().Unix()})
}

func (s *Server) Authorize(w http.ResponseWriter, r *http.Request) {
	params, client, err := s.validateAuthorize(r.Context(), r.URL.Query())
	if err != nil {
		s.redirectOAuthError(w, r, params, err)
		return
	}
	identity, err := s.Auth.IdentityRequest(r)
	if err != nil {
		returnTo := s.PublicURL + r.URL.RequestURI()
		http.Redirect(w, r, "/?returnTo="+url.QueryEscape(returnTo), http.StatusFound)
		return
	}
	csrfCookie, err := r.Cookie(auth.CSRFCookie)
	if err != nil || csrfCookie.Value == "" {
		csrf := auth.NewCSRFToken()
		http.SetCookie(w, &http.Cookie{Name: auth.CSRFCookie, Value: csrf, Path: "/", Secure: strings.HasPrefix(s.PublicURL, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: 1800})
		csrfCookie = &http.Cookie{Value: csrf}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = consentTemplate.Execute(w, map[string]any{"ClientName": client.Name, "Username": identity.Username, "Scope": params.Get("scope"), "Params": params, "CSRF": csrfCookie.Value})
}

func (s *Server) AuthorizeDecision(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "Invalid form submission.")
		return
	}
	params := url.Values{}
	for _, key := range []string{"client_id", "redirect_uri", "response_type", "scope", "state", "code_challenge", "code_challenge_method", "resource"} {
		params.Set(key, r.Form.Get(key))
	}
	clientParams, _, err := s.validateAuthorize(r.Context(), params)
	if err != nil {
		s.redirectOAuthError(w, r, params, err)
		return
	}
	identity, err := s.Auth.IdentityRequest(r)
	if err != nil {
		oauthError(w, http.StatusUnauthorized, "login_required", "Sign in before authorizing this client.")
		return
	}
	csrfCookie, cookieErr := r.Cookie(auth.CSRFCookie)
	if cookieErr != nil || !constantEqual(csrfCookie.Value, r.Form.Get("csrf")) {
		oauthError(w, http.StatusForbidden, "access_denied", "Consent form expired. Try again.")
		return
	}
	if r.Form.Get("decision") != "approve" {
		s.redirectWithError(w, r, params, "access_denied", "The resource owner denied the request.")
		return
	}
	code := random(48)
	_, err = s.Store.DB.ExecContext(r.Context(), `INSERT INTO oauth_codes(id,code_hash,client_id,user_id,redirect_uri,scope,resource,code_challenge,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, uuid.NewString(), auth.HashToken(code), clientParams.Get("client_id"), identity.UserID, clientParams.Get("redirect_uri"), clientParams.Get("scope"), clientParams.Get("resource"), clientParams.Get("code_challenge"), time.Now().UTC().Add(10*time.Minute).Format(time.RFC3339Nano), store.Now())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	_ = s.Store.Audit(r.Context(), "user", identity.UserID, "oauth.authorize", "oauth_client", clientParams.Get("client_id"), "Authorized MCP client", httpx.ClientIP(r), r.UserAgent())
	destination, _ := url.Parse(clientParams.Get("redirect_uri"))
	query := destination.Query()
	query.Set("code", code)
	if state := clientParams.Get("state"); state != "" {
		query.Set("state", state)
	}
	query.Set("iss", s.PublicURL)
	destination.RawQuery = query.Encode()
	http.Redirect(w, r, destination.String(), http.StatusFound)
}

func (s *Server) Token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "Invalid token request.")
		return
	}
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r)
	case "refresh_token":
		s.refreshToken(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "Supported grants are authorization_code and refresh_token.")
	}
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request) {
	code := r.Form.Get("code")
	clientID := r.Form.Get("client_id")
	redirectURI := r.Form.Get("redirect_uri")
	verifier := r.Form.Get("code_verifier")
	resource := r.Form.Get("resource")
	if code == "" || clientID == "" || redirectURI == "" || verifier == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "code, client_id, redirect_uri, and code_verifier are required.")
		return
	}
	var id, userID, storedClient, storedRedirect, scope, storedResource, challenge, expires string
	var used sql.NullString
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT id,user_id,client_id,redirect_uri,scope,resource,code_challenge,expires_at,used_at FROM oauth_codes WHERE code_hash=?`, auth.HashToken(code)).Scan(&id, &userID, &storedClient, &storedRedirect, &scope, &storedResource, &challenge, &expires, &used)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "Authorization code is invalid.")
		return
	}
	expiry, _ := time.Parse(time.RFC3339Nano, expires)
	digest := sha256.Sum256([]byte(verifier))
	actual := base64.RawURLEncoding.EncodeToString(digest[:])
	if used.Valid || time.Now().After(expiry) || storedClient != clientID || storedRedirect != redirectURI || storedResource != resource || resource != s.Resource || !constantEqual(challenge, actual) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "Authorization code is invalid or expired.")
		return
	}
	result, err := s.Store.DB.ExecContext(r.Context(), `UPDATE oauth_codes SET used_at=? WHERE id=? AND used_at IS NULL`, store.Now(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "Authorization code was already used.")
		return
	}
	s.issueTokens(w, r.Context(), userID, clientID, scope, resource, uuid.NewString())
}

func (s *Server) refreshToken(w http.ResponseWriter, r *http.Request) {
	token := r.Form.Get("refresh_token")
	clientID := r.Form.Get("client_id")
	resource := r.Form.Get("resource")
	if token == "" || clientID == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "refresh_token and client_id are required.")
		return
	}
	var id, userID, storedClient, scope, storedResource, family, expires string
	var used, revoked sql.NullString
	err := s.Store.DB.QueryRowContext(r.Context(), `SELECT id,user_id,client_id,scope,resource,family_id,expires_at,used_at,revoked_at FROM oauth_tokens WHERE token_hash=?`, auth.HashToken(token)).Scan(&id, &userID, &storedClient, &scope, &storedResource, &family, &expires, &used, &revoked)
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "Refresh token is invalid.")
		return
	}
	expiry, _ := time.Parse(time.RFC3339Nano, expires)
	if resource == "" {
		resource = storedResource
	}
	if used.Valid || revoked.Valid || time.Now().After(expiry) || storedClient != clientID || storedResource != resource {
		_, _ = s.Store.DB.ExecContext(r.Context(), `UPDATE oauth_tokens SET revoked_at=? WHERE family_id=? AND revoked_at IS NULL`, store.Now(), family)
		oauthError(w, http.StatusBadRequest, "invalid_grant", "Refresh token is invalid, expired, or reused.")
		return
	}
	result, err := s.Store.DB.ExecContext(r.Context(), `UPDATE oauth_tokens SET used_at=? WHERE id=? AND used_at IS NULL AND revoked_at IS NULL`, store.Now(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "Refresh token was already used.")
		return
	}
	s.issueTokens(w, r.Context(), userID, clientID, scope, resource, family)
}

func (s *Server) issueTokens(w http.ResponseWriter, ctx context.Context, userID, clientID, scope, resource, family string) {
	var username string
	if err := s.Store.DB.QueryRowContext(ctx, `SELECT username FROM users WHERE id=?`, userID).Scan(&username); err != nil {
		writeStoreError(w, err)
		return
	}
	access, _, err := s.Auth.Signer().Sign(userID, resource, username, scope, "oauth", 10*time.Minute)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	refresh := random(48)
	now := time.Now().UTC()
	_, err = s.Store.DB.ExecContext(ctx, `INSERT INTO oauth_tokens(id,token_hash,client_id,user_id,scope,resource,family_id,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, uuid.NewString(), auth.HashToken(refresh), clientID, userID, scope, resource, family, now.Add(30*24*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	httpx.JSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 600, "refresh_token": refresh, "scope": scope, "resource": resource})
}

func (s *Server) Revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	token := r.Form.Get("token")
	if token != "" {
		_, _ = s.Store.DB.ExecContext(r.Context(), `UPDATE oauth_tokens SET revoked_at=? WHERE token_hash=? AND revoked_at IS NULL`, store.Now(), auth.HashToken(token))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) VerifyMCP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && origin != s.PublicURL {
			httpx.Error(w, http.StatusForbidden, "invalid_origin", "Cross-origin MCP request rejected.")
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if token == "" {
			s.challenge(w, "asgard:read")
			return
		}
		claims, err := s.Auth.Signer().Verify(token, s.Resource, "oauth")
		if err != nil {
			s.challenge(w, "asgard:read")
			return
		}
		identity := auth.Identity{UserID: claims.Subject, Username: claims.Username, ActorType: "agent", Scope: claims.Scope, Claims: claims}
		next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), identity)))
	})
}
func (s *Server) challenge(w http.ResponseWriter, scope string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp", scope="%s"`, s.PublicURL, scope))
	httpx.Error(w, http.StatusUnauthorized, "authentication_required", "OAuth bearer token is required.")
}

func (s *Server) validateAuthorize(ctx context.Context, params url.Values) (url.Values, Client, error) {
	clientID := params.Get("client_id")
	redirect := params.Get("redirect_uri")
	if clientID == "" || redirect == "" || params.Get("response_type") != "code" {
		return params, Client{}, oauthValidationError{"invalid_request", "client_id, redirect_uri, and response_type=code are required."}
	}
	client, err := s.client(ctx, clientID)
	if err != nil {
		return params, Client{}, oauthValidationError{"invalid_request", "Unknown or invalid client."}
	}
	if !contains(client.RedirectURIs, redirect) {
		return params, client, oauthValidationError{"invalid_request", "redirect_uri is not registered for this client."}
	}
	if params.Get("code_challenge_method") != "S256" || len(params.Get("code_challenge")) < 43 {
		return params, client, oauthValidationError{"invalid_request", "PKCE with code_challenge_method S256 is required."}
	}
	if params.Get("resource") != s.Resource {
		return params, client, oauthValidationError{"invalid_target", "The MCP resource parameter is required."}
	}
	scope := normalizeScope(params.Get("scope"))
	if scope == "" {
		scope = "asgard:read"
	}
	if !validScope(scope) {
		return params, client, oauthValidationError{"invalid_scope", "One or more requested scopes are unsupported."}
	}
	params.Set("scope", scope)
	return params, client, nil
}

func (s *Server) client(ctx context.Context, clientID string) (Client, error) {
	var client Client
	var redirects, grants string
	err := s.Store.DB.QueryRowContext(ctx, `SELECT id,client_id,client_name,redirect_uris,grant_types FROM oauth_clients WHERE client_id=?`, clientID).Scan(&client.ID, &client.ClientID, &client.Name, &redirects, &grants)
	if err == nil {
		_ = json.Unmarshal([]byte(redirects), &client.RedirectURIs)
		_ = json.Unmarshal([]byte(grants), &client.GrantTypes)
		return client, nil
	}
	if !strings.HasPrefix(clientID, "https://") {
		return client, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return client, err
	}
	response, err := s.secureClient.Do(request)
	if err != nil {
		return client, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return client, fmt.Errorf("metadata returned %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return client, err
	}
	var metadata struct {
		ClientID     string   `json:"client_id"`
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
		GrantTypes   []string `json:"grant_types"`
	}
	if err = json.Unmarshal(body, &metadata); err != nil || metadata.ClientID != clientID || metadata.ClientName == "" || len(metadata.RedirectURIs) == 0 {
		return client, errors.New("invalid client metadata document")
	}
	for _, redirect := range metadata.RedirectURIs {
		if err := validateRedirectURI(redirect); err != nil {
			return client, err
		}
	}
	redirectJSON, _ := json.Marshal(unique(metadata.RedirectURIs))
	if len(metadata.GrantTypes) == 0 {
		metadata.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	grantJSON, _ := json.Marshal(unique(metadata.GrantTypes))
	client = Client{ID: uuid.NewString(), ClientID: clientID, Name: metadata.ClientName, RedirectURIs: unique(metadata.RedirectURIs), GrantTypes: unique(metadata.GrantTypes)}
	_, err = s.Store.DB.ExecContext(ctx, `INSERT OR IGNORE INTO oauth_clients(id,client_id,client_name,redirect_uris,grant_types,token_endpoint_auth_method,created_at) VALUES(?,?,?,?,?,'none',?)`, client.ID, client.ClientID, client.Name, string(redirectJSON), string(grantJSON), store.Now())
	return client, err
}

type oauthValidationError struct{ Code, Description string }

func (e oauthValidationError) Error() string { return e.Description }
func (s *Server) redirectOAuthError(w http.ResponseWriter, r *http.Request, params url.Values, err error) {
	validation := oauthValidationError{"invalid_request", err.Error()}
	if typed, ok := err.(oauthValidationError); ok {
		validation = typed
	}
	if redirect := params.Get("redirect_uri"); redirect != "" && validateRedirectURI(redirect) == nil {
		s.redirectWithError(w, r, params, validation.Code, validation.Description)
		return
	}
	oauthError(w, http.StatusBadRequest, validation.Code, validation.Description)
}
func (s *Server) redirectWithError(w http.ResponseWriter, r *http.Request, params url.Values, code, description string) {
	destination, err := url.Parse(params.Get("redirect_uri"))
	if err != nil {
		oauthError(w, http.StatusBadRequest, code, description)
		return
	}
	query := destination.Query()
	query.Set("error", code)
	query.Set("error_description", description)
	if state := params.Get("state"); state != "" {
		query.Set("state", state)
	}
	destination.RawQuery = query.Encode()
	http.Redirect(w, r, destination.String(), http.StatusFound)
}

func validateRedirectURI(raw string) error {
	if len(raw) > 2048 {
		return errors.New("redirect URI is too long")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Fragment != "" || parsed.User != nil {
		return errors.New("redirect URI is invalid")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "https" {
		if parsed.Hostname() == "" {
			return errors.New("HTTPS redirect needs a host")
		}
		return nil
	}
	if scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1" || parsed.Hostname() == "localhost") {
		return nil
	}
	if scheme != "http" && scheme != "javascript" && scheme != "data" && scheme != "file" && strings.Contains(scheme, ".") {
		return nil
	}
	return errors.New("redirect URI must use HTTPS, a loopback HTTP address, or a private-use reverse-domain scheme")
}
func normalizeScope(value string) string {
	set := map[string]bool{}
	for _, scope := range strings.Fields(value) {
		set[scope] = true
	}
	out := make([]string, 0, len(set))
	for scope := range set {
		out = append(out, scope)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}
func validScope(value string) bool {
	allowed := map[string]bool{}
	for _, scope := range strings.Fields(allScopes) {
		allowed[scope] = true
	}
	for _, scope := range strings.Fields(value) {
		if !allowed[scope] {
			return false
		}
	}
	return true
}
func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
func unique(items []string) []string {
	set := map[string]bool{}
	out := []string{}
	for _, item := range items {
		if !set[item] {
			set[item] = true
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}
func random(size int) string {
	bytes := make([]byte, size)
	_, _ = rand.Read(bytes)
	return base64.RawURLEncoding.EncodeToString(bytes)
}
func constantEqual(a, b string) bool {
	if len(a) != len(b) || a == "" {
		return false
	}
	var value byte
	for i := range a {
		value |= a[i] ^ b[i]
	}
	return value == 0
}
func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, status, map[string]any{"error": code, "error_description": description})
}
func writeStoreError(w http.ResponseWriter, err error) {
	oauthError(w, http.StatusInternalServerError, "server_error", err.Error())
}

func safeHTTPClient() *http.Client {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: func(ctx context.Context, networkName, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, item := range ips {
			ip := item.IP
			if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, networkName, net.JoinHostPort(ip.String(), port))
		}
		return nil, errors.New("client metadata host has no public address")
	}}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
}

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Authorize agent · Asgard</title><style>:root{color-scheme:light}*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:#f2f1ed;color:#26251e;font:16px/1.5 system-ui,sans-serif}.card{width:min(440px,calc(100% - 32px));padding:32px;background:#f7f7f4;border:1px solid rgba(38,37,30,.16);border-radius:16px;box-shadow:0 28px 70px rgba(0,0,0,.14)}h1{margin:0 0 8px;font-size:28px;letter-spacing:-.4px}p{color:rgba(38,37,30,.72)}code{font:13px ui-monospace,monospace;background:#e6e5e0;padding:3px 6px;border-radius:4px}.scope{padding:16px;background:#ebeae5;border-radius:8px;margin:24px 0}.actions{display:flex;gap:12px;justify-content:flex-end}button{min-height:44px;padding:0 18px;border-radius:8px;border:1px solid rgba(38,37,30,.18);font:600 14px system-ui;cursor:pointer;white-space:nowrap}.deny{background:transparent}.approve{background:#26251e;color:#f7f7f4}.approve:focus,.deny:focus{outline:3px solid #c63f00;outline-offset:3px}</style></head><body><main class="card"><p style="margin:0;color:#c63f00;font-weight:700">ASGARD AGENT ACCESS</p><h1>Authorize {{.ClientName}}</h1><p>Signed in as <strong>{{.Username}}</strong>. This client is asking to interact with your Asgard control plane.</p><div class="scope"><strong>Requested access</strong><br><code>{{.Scope}}</code></div><form method="post" action="/oauth/authorize">{{range $key,$values := .Params}}{{range $values}}<input type="hidden" name="{{$key}}" value="{{.}}">{{end}}{{end}}<input type="hidden" name="csrf" value="{{.CSRF}}"><div class="actions"><button class="deny" name="decision" value="deny">Deny</button><button class="approve" name="decision" value="approve">Authorize</button></div></form></main></body></html>`))
