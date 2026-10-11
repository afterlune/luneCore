package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/go-keyring/keyring"
)

const (
	oauthKeyringService = "Lune Agent MCP OAuth"
	oauthCallbackPath   = "/oauth/callback"
	oauthCIMDPort       = "42477"
)

type oauthRecord struct {
	Issuer        string `json:"issuer"`
	ClientID      string `json:"client_id"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	TokenType     string `json:"token_type,omitempty"`
	Scope         string `json:"scope,omitempty"`
	Resource      string `json:"resource"`
	TokenEndpoint string `json:"token_endpoint"`
	ExpiresAt     int64  `json:"expires_at,omitempty"`
}

type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

type authorizationServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported"`
	AuthorizationResponseISSSupported bool     `json:"authorization_response_iss_parameter_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	ScopesSupported                   []string `json:"scopes_supported"`
}

type oauthCallback struct {
	Code   string
	State  string
	Issuer string
	Error  string
}

func (client *httpTransport) Authorize(ctx context.Context) error {
	if ctx == nil {
		return errors.New("OAuth authorization requires a context")
	}
	flowContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	resourceMetadata, scopes, err := client.discoverOAuth(flowContext)
	if err != nil {
		return err
	}
	issuer := resourceMetadata.AuthorizationServers[0]
	metadata, err := discoverAuthorizationServer(flowContext, issuer)
	if err != nil {
		return err
	}
	if len(metadata.CodeChallengeMethodsSupported) > 0 && !containsString(metadata.CodeChallengeMethodsSupported, "S256") {
		return errors.New("MCP authorization server does not support the required PKCE S256 method")
	}
	previous, err := client.loadOAuthRecord()
	if err != nil {
		return fmt.Errorf("load existing MCP OAuth credentials from the system keyring: %w", err)
	}
	if previous != nil && previous.Issuer == metadata.Issuer {
		scopes = mergeScopes(strings.Fields(previous.Scope), scopes)
	}
	cimd := client.clientMetadataURL != "" && metadata.ClientIDMetadataDocumentSupported
	listener, redirectURI, err := openOAuthCallbackListener(cimd)
	if err != nil {
		if !cimd || metadata.RegistrationEndpoint == "" {
			return err
		}
		listener, redirectURI, err = openOAuthCallbackListener(false)
		if err != nil {
			return err
		}
	}
	defer listener.Close()
	clientID, err := client.registerOAuthClient(flowContext, metadata, redirectURI)
	if err != nil && redirectURI == oauthRedirectURI(oauthCIMDPort) && metadata.RegistrationEndpoint != "" {
		_ = listener.Close()
		listener, redirectURI, err = openOAuthCallbackListener(false)
		if err == nil {
			defer listener.Close()
			clientID, err = client.registerOAuthClient(flowContext, metadata, redirectURI)
		}
	}
	if err != nil {
		return err
	}
	state, err := randomToken(32)
	if err != nil {
		return err
	}
	callback, shutdown, err := serveOAuthCallback(listener, redirectURI, state)
	if err != nil {
		return err
	}
	defer shutdown()
	verifier, err := randomToken(32)
	if err != nil {
		return err
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challengeBytes[:])},
		"code_challenge_method": {"S256"},
		"resource":              {canonicalResource(client.endpoint)},
	}
	if len(scopes) > 0 {
		query.Set("scope", strings.Join(scopes, " "))
	}
	parsedAuthorization, err := url.Parse(metadata.AuthorizationEndpoint)
	if err != nil {
		return errors.New("MCP authorization server has an invalid authorization endpoint")
	}
	authorizationQuery := parsedAuthorization.Query()
	for key, values := range query {
		for _, value := range values {
			authorizationQuery.Add(key, value)
		}
	}
	parsedAuthorization.RawQuery = authorizationQuery.Encode()
	authorizationURL := parsedAuthorization.String()
	if err := openOAuthBrowser(authorizationURL); err != nil {
		return fmt.Errorf("could not open the system browser; open this authorization URL manually: %s (%w)", authorizationURL, err)
	}
	var response oauthCallback
	select {
	case response = <-callback:
	case <-flowContext.Done():
		return errors.New("MCP OAuth authorization timed out or was canceled")
	}
	if response.State != state {
		return errors.New("MCP OAuth callback state did not match the authorization request")
	}
	if response.Issuer != "" && response.Issuer != metadata.Issuer || metadata.AuthorizationResponseISSSupported && response.Issuer == "" {
		return errors.New("MCP OAuth callback issuer did not match the discovered authorization server")
	}
	if response.Error != "" || response.Code == "" {
		return errors.New("MCP OAuth authorization was denied or returned no authorization code")
	}
	token, err := client.exchangeAuthorizationCode(flowContext, metadata, clientID, redirectURI, verifier, response.Code)
	if err != nil {
		return err
	}
	token.Issuer = metadata.Issuer
	token.ClientID = clientID
	token.Resource = canonicalResource(client.endpoint)
	token.TokenEndpoint = metadata.TokenEndpoint
	if token.Scope == "" {
		token.Scope = strings.Join(scopes, " ")
	}
	return client.saveOAuthRecord(token)
}

func (client *httpTransport) discoverOAuth(ctx context.Context) (protectedResourceMetadata, []string, error) {
	requestBody, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list", Params: requestParams(nil)})
	if err != nil {
		return protectedResourceMetadata{}, nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, strings.NewReader(string(requestBody)))
	if err != nil {
		return protectedResourceMetadata{}, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", protocolVersion)
	request.Header.Set("Mcp-Method", "tools/list")
	response, err := client.client.Do(request)
	if err != nil {
		return protectedResourceMetadata{}, nil, fmt.Errorf("probe MCP server authorization: %w", err)
	}
	challenge := response.Header.Get("WWW-Authenticate")
	status := response.StatusCode
	_ = response.Body.Close()
	if status != http.StatusUnauthorized {
		if status >= 200 && status < 300 {
			return protectedResourceMetadata{}, nil, errors.New("MCP server does not require OAuth authorization")
		}
		return protectedResourceMetadata{}, nil, fmt.Errorf("MCP server authorization probe returned HTTP %d", status)
	}
	challengeValues := parseBearerChallenge(challenge)
	metadataURL := challengeValues["resource_metadata"]
	var candidates []string
	if metadataURL != "" {
		candidates = []string{metadataURL}
	} else {
		parsed, parseErr := url.Parse(client.endpoint)
		if parseErr != nil {
			return protectedResourceMetadata{}, nil, errors.New("invalid MCP server URL")
		}
		endpointPath := strings.Trim(parsed.Path, "/")
		pathCandidate := *parsed
		pathCandidate.Path = "/.well-known/oauth-protected-resource"
		if endpointPath != "" {
			pathCandidate.Path += "/" + endpointPath
		}
		pathCandidate.RawPath = ""
		rootCandidate := *parsed
		rootCandidate.Path = "/.well-known/oauth-protected-resource"
		rootCandidate.RawPath = ""
		pathCandidate.RawQuery, rootCandidate.RawQuery = "", ""
		candidates = []string{pathCandidate.String(), rootCandidate.String()}
	}
	var protected protectedResourceMetadata
	var lastErr error
	for _, candidate := range candidates {
		if !validOAuthURL(candidate) {
			lastErr = errors.New("protected resource metadata URL must use HTTPS")
			continue
		}
		if err := getOAuthJSON(ctx, client.client, candidate, &protected); err != nil {
			lastErr = err
			continue
		}
		if len(protected.AuthorizationServers) == 0 {
			lastErr = errors.New("protected resource metadata did not list an authorization server")
			continue
		}
		if protected.Resource != "" && canonicalResource(protected.Resource) != canonicalResource(client.endpoint) {
			return protectedResourceMetadata{}, nil, errors.New("protected resource metadata does not match the configured MCP endpoint")
		}
		for _, issuer := range protected.AuthorizationServers {
			if !validOAuthURL(issuer) {
				return protectedResourceMetadata{}, nil, errors.New("protected resource metadata listed an insecure authorization server")
			}
		}
		challengeScope := strings.Fields(challengeValues["scope"])
		if len(challengeScope) > 0 {
			return protected, challengeScope, nil
		}
		return protected, protected.ScopesSupported, nil
	}
	if lastErr == nil {
		lastErr = errors.New("protected resource metadata could not be discovered")
	}
	return protectedResourceMetadata{}, nil, lastErr
}

func discoverAuthorizationServer(ctx context.Context, issuer string) (authorizationServerMetadata, error) {
	parsed, err := url.Parse(issuer)
	if err != nil || !validOAuthURL(issuer) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return authorizationServerMetadata{}, errors.New("authorization server issuer must be a secure URL without query or fragment")
	}
	path := strings.Trim(parsed.EscapedPath(), "/")
	var candidates []string
	base := *parsed
	base.RawQuery, base.Fragment = "", ""
	base.Path, base.RawPath = "", ""
	if path != "" {
		candidates = []string{
			strings.TrimSuffix(base.String(), "/") + "/.well-known/oauth-authorization-server/" + path,
			strings.TrimSuffix(base.String(), "/") + "/.well-known/openid-configuration/" + path,
			strings.TrimSuffix(parsed.String(), "/") + "/.well-known/openid-configuration",
		}
	} else {
		candidates = []string{
			strings.TrimSuffix(base.String(), "/") + "/.well-known/oauth-authorization-server",
			strings.TrimSuffix(base.String(), "/") + "/.well-known/openid-configuration",
		}
	}
	var lastErr error
	for _, candidate := range candidates {
		var metadata authorizationServerMetadata
		if err := getOAuthJSON(ctx, nil, candidate, &metadata); err != nil {
			lastErr = err
			continue
		}
		if metadata.Issuer != issuer {
			return authorizationServerMetadata{}, errors.New("authorization server issuer did not match its discovery URL")
		}
		if !validOAuthURL(metadata.AuthorizationEndpoint) || !validOAuthURL(metadata.TokenEndpoint) {
			return authorizationServerMetadata{}, errors.New("authorization server metadata contains an insecure endpoint")
		}
		if metadata.RegistrationEndpoint != "" && !validOAuthURL(metadata.RegistrationEndpoint) {
			return authorizationServerMetadata{}, errors.New("authorization server registration endpoint is insecure")
		}
		return metadata, nil
	}
	if lastErr == nil {
		lastErr = errors.New("authorization server metadata could not be discovered")
	}
	return authorizationServerMetadata{}, lastErr
}

func (client *httpTransport) registerOAuthClient(ctx context.Context, metadata authorizationServerMetadata, redirectURI string) (string, error) {
	if client.clientMetadataURL != "" && metadata.ClientIDMetadataDocumentSupported {
		var document struct {
			ClientID                string   `json:"client_id"`
			RedirectURIs            []string `json:"redirect_uris"`
			TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		}
		if err := getOAuthJSON(ctx, nil, client.clientMetadataURL, &document); err == nil && document.ClientID == client.clientMetadataURL && containsString(document.RedirectURIs, redirectURI) && (document.TokenEndpointAuthMethod == "" || document.TokenEndpointAuthMethod == "none") {
			return client.clientMetadataURL, nil
		}
	}
	if metadata.RegistrationEndpoint == "" {
		return "", errors.New("authorization server supports neither the configured client metadata URL nor dynamic client registration")
	}
	registration := map[string]any{
		"client_name": "Lune Agent", "application_type": "native",
		"redirect_uris": []string{redirectURI}, "grant_types": []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"}, "token_endpoint_auth_method": "none",
	}
	body, _ := json.Marshal(registration)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, metadata.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := oauthHTTPClient().Do(request)
	if err != nil {
		return "", fmt.Errorf("register OAuth client: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("OAuth dynamic client registration returned HTTP %d", response.StatusCode)
	}
	var result struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil || result.ClientID == "" {
		return "", errors.New("OAuth dynamic client registration returned no client ID")
	}
	return result.ClientID, nil
}

func serveOAuthCallback(listener net.Listener, redirectURI, expectedState string) (<-chan oauthCallback, func(), error) {
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return nil, nil, err
	}
	result := make(chan oauthCallback, 1)
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != parsed.Path {
			http.NotFound(writer, request)
			return
		}
		query := request.URL.Query()
		state := query.Get("state")
		if state == "" || state != expectedState {
			http.Error(writer, "Invalid authorization state.", http.StatusBadRequest)
			return
		}
		select {
		case result <- oauthCallback{Code: query.Get("code"), State: state, Issuer: query.Get("iss"), Error: query.Get("error")}:
			_, _ = writer.Write([]byte("Authorization received. Return to Lune Agent."))
		default:
			_, _ = writer.Write([]byte("Authorization was already received."))
		}
	})}
	go func() { _ = server.Serve(listener) }()
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
	return result, shutdown, nil
}

func (client *httpTransport) exchangeAuthorizationCode(ctx context.Context, metadata authorizationServerMetadata, clientID, redirectURI, verifier, code string) (*oauthRecord, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {canonicalResource(client.endpoint)},
	}
	response, err := postOAuthForm(ctx, metadata.TokenEndpoint, form)
	if err != nil {
		return nil, fmt.Errorf("exchange MCP authorization code: %w", err)
	}
	return decodeOAuthToken(response)
}

func (client *httpTransport) refresh(ctx context.Context, previous *oauthRecord) (*oauthRecord, error) {
	client.oauthMu.Lock()
	defer client.oauthMu.Unlock()
	if client.oauth != nil && client.oauth.AccessToken != previous.AccessToken {
		copy := *client.oauth
		return &copy, nil
	}
	if previous.RefreshToken == "" {
		return nil, errors.New("no refresh token is available; run /mcp auth <name> again")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {previous.RefreshToken},
		"client_id":     {previous.ClientID},
		"resource":      {previous.Resource},
	}
	response, err := postOAuthForm(ctx, previous.TokenEndpoint, form)
	if err != nil {
		return nil, fmt.Errorf("OAuth token refresh failed; run /mcp auth <name> again: %w", err)
	}
	updated, err := decodeOAuthToken(response)
	if err != nil {
		return nil, err
	}
	updated.Issuer, updated.ClientID, updated.Resource, updated.TokenEndpoint = previous.Issuer, previous.ClientID, previous.Resource, previous.TokenEndpoint
	if updated.RefreshToken == "" {
		updated.RefreshToken = previous.RefreshToken
	}
	if updated.Scope == "" {
		updated.Scope = previous.Scope
	}
	if err := client.saveOAuthRecordLocked(updated); err != nil {
		return nil, err
	}
	return updated, nil
}

func decodeOAuthToken(response *http.Response) (*oauthRecord, error) {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("OAuth token endpoint returned HTTP %d", response.StatusCode)
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token); err != nil || token.AccessToken == "" {
		return nil, errors.New("OAuth token endpoint returned no access token")
	}
	if token.TokenType != "" && !strings.EqualFold(token.TokenType, "Bearer") {
		return nil, errors.New("OAuth token endpoint returned an unsupported token type")
	}
	record := &oauthRecord{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, TokenType: token.TokenType, Scope: token.Scope}
	if token.ExpiresIn > 0 {
		record.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).Unix()
	}
	return record, nil
}

func postOAuthForm(ctx context.Context, endpoint string, form url.Values) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := oauthHTTPClient().Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return response, nil
}

func (client *httpTransport) loadOAuthRecord() (*oauthRecord, error) {
	client.oauthMu.Lock()
	if client.oauthLoaded {
		if client.oauth == nil {
			client.oauthMu.Unlock()
			return nil, nil
		}
		copy := *client.oauth
		client.oauthMu.Unlock()
		return &copy, nil
	}
	client.oauthMu.Unlock()
	account := oauthAccount(client.serverName, canonicalResource(client.endpoint))
	data, err := keyring.Get(oauthKeyringService, account)
	if errors.Is(err, keyring.ErrNotFound) {
		client.oauthMu.Lock()
		client.oauthLoaded = true
		client.oauth = nil
		client.oauthMu.Unlock()
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record oauthRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, errors.New("system keyring contains an invalid MCP OAuth record")
	}
	if record.AccessToken == "" || record.Resource != canonicalResource(client.endpoint) {
		return nil, errors.New("system keyring MCP OAuth record does not match this server")
	}
	client.oauthMu.Lock()
	client.oauthLoaded = true
	client.oauth = &record
	copy := record
	client.oauthMu.Unlock()
	return &copy, nil
}

func (client *httpTransport) saveOAuthRecord(record *oauthRecord) error {
	client.oauthMu.Lock()
	defer client.oauthMu.Unlock()
	return client.saveOAuthRecordLocked(record)
}

func (client *httpTransport) saveOAuthRecordLocked(record *oauthRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := keyring.Set(oauthKeyringService, oauthAccount(client.serverName, canonicalResource(client.endpoint)), data); err != nil {
		return fmt.Errorf("store MCP OAuth credentials in the system keyring: %w", err)
	}
	client.oauth = record
	client.oauthLoaded = true
	return nil
}

func oauthAccount(serverName, resource string) string {
	sum := sha256.Sum256([]byte(serverName + "\x00" + resource))
	return "mcp-" + base64.RawURLEncoding.EncodeToString(sum[:12])
}

func mergeScopes(existing, requested []string) []string {
	seen := make(map[string]bool, len(existing)+len(requested))
	result := make([]string, 0, len(existing)+len(requested))
	for _, scope := range append(append([]string(nil), existing...), requested...) {
		if scope != "" && !seen[scope] {
			seen[scope] = true
			result = append(result, scope)
		}
	}
	return result
}

func canonicalResource(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	parsed.Fragment, parsed.RawFragment = "", ""
	if parsed.Path == "/" {
		parsed.Path, parsed.RawPath = "", ""
	}
	return parsed.String()
}

func openOAuthCallbackListener(cimd bool) (net.Listener, string, error) {
	address := "127.0.0.1:0"
	if cimd {
		address = net.JoinHostPort("127.0.0.1", oauthCIMDPort)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, "", fmt.Errorf("open loopback OAuth callback listener: %w", err)
	}
	return listener, "http://" + listener.Addr().String() + oauthCallbackPath, nil
}

func oauthRedirectURI(port string) string {
	return "http://127.0.0.1:" + port + oauthCallbackPath
}

func openOAuthBrowser(target string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", target}
	case "darwin":
		command, args = "open", []string{target}
	default:
		command, args = "xdg-open", []string{target}
	}
	process := exec.Command(command, args...)
	if err := process.Start(); err != nil {
		return err
	}
	go func() { _ = process.Wait() }()
	return nil
}

func randomToken(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func parseBearerChallenge(value string) map[string]string {
	var result = make(map[string]string)
	trimmed := strings.TrimSpace(value)
	if len(trimmed) >= 6 && strings.EqualFold(trimmed[:6], "Bearer") {
		trimmed = strings.TrimSpace(trimmed[6:])
	}
	for _, entry := range splitOAuthParams(trimmed) {
		key, value, found := strings.Cut(strings.TrimSpace(entry), "=")
		if found {
			value = strings.TrimSpace(value)
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
			result[strings.ToLower(strings.TrimSpace(key))] = value
		}
	}
	return result
}

func splitOAuthParams(value string) []string {
	var parts []string
	start := 0
	quoted, escaped := false, false
	for index, char := range value {
		if escaped {
			escaped = false
			continue
		}
		if quoted && char == '\\' {
			escaped = true
			continue
		}
		if char == '"' {
			quoted = !quoted
		} else if char == ',' && !quoted {
			parts = append(parts, value[start:index])
			start = index + 1
		}
	}
	return append(parts, value[start:])
}

func validOAuthURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func getOAuthJSON(ctx context.Context, httpClient *http.Client, target string, destination any) error {
	if !validOAuthURL(target) {
		return errors.New("OAuth discovery URL must use HTTPS or loopback HTTP")
	}
	if httpClient == nil {
		httpClient = oauthHTTPClient()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("OAuth discovery endpoint returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(destination); err != nil {
		return fmt.Errorf("decode OAuth discovery response: %w", err)
	}
	return nil
}

func oauthHTTPClient() *http.Client {
	return &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
