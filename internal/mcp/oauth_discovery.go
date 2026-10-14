package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

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
