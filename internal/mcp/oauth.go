package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

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
