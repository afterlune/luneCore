package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-keyring/keyring"
)

const oauthKeyringService = "Lune Agent MCP OAuth"

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
