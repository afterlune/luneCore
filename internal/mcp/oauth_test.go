package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	projectconfig "github.com/afterlune/luneCore/internal/config"
)

func TestParseBearerChallenge(t *testing.T) {
	values := parseBearerChallenge(`Bearer realm="mcp", resource_metadata="https://auth.example/meta", scope="files:read write"`)
	if values["resource_metadata"] != "https://auth.example/meta" || values["scope"] != "files:read write" {
		t.Fatalf("parsed challenge = %#v", values)
	}
}

func TestMergeScopesPreservesExistingAndAddsChallengeScopesOnce(t *testing.T) {
	got := mergeScopes([]string{"files:read", "profile"}, []string{"profile", "files:write"})
	if strings.Join(got, " ") != "files:read profile files:write" {
		t.Fatalf("merged scopes = %v", got)
	}
}

func TestOAuthURLValidationAllowsOnlyHTTPSOrLoopbackHTTP(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"https://auth.example.test/oauth", true},
		{"http://127.0.0.1:8080/oauth", true},
		{"http://localhost/oauth", true},
		{"http://auth.example.test/oauth", false},
		{"https://user:secret@auth.example.test/oauth", false},
		{"https://auth.example.test/oauth#fragment", false},
		{"file:///tmp/metadata.json", false},
	}
	for _, test := range tests {
		if got := validOAuthURL(test.url); got != test.want {
			t.Errorf("validOAuthURL(%q) = %v, want %v", test.url, got, test.want)
		}
	}
}

func TestDiscoverOAuthUsesResourceMetadataAndChallengeScopes(t *testing.T) {
	var endpoint string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/mcp":
			writer.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+endpoint+`/metadata", scope="files:read write"`)
			writer.WriteHeader(http.StatusUnauthorized)
		case request.Method == http.MethodGet && request.URL.Path == "/metadata":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"resource": endpoint + "/mcp", "authorization_servers": []string{endpoint + "/tenant"},
				"scopes_supported": []string{"fallback"},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	endpoint = server.URL
	client := newHTTPTransport("test", structMCPHTTPConfig(endpoint+"/mcp"), nil, nil)
	metadata, scopes, err := client.discoverOAuth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Resource != endpoint+"/mcp" || len(scopes) != 2 || scopes[0] != "files:read" || scopes[1] != "write" {
		t.Fatalf("metadata=%#v scopes=%v", metadata, scopes)
	}
}

func TestDiscoverAuthorizationServerRequiresMatchingIssuer(t *testing.T) {
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/.well-known/oauth-authorization-server/tenant" {
			http.NotFound(writer, request)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"issuer": issuer, "authorization_endpoint": serverEndpoint(request, "/authorize"),
			"token_endpoint": serverEndpoint(request, "/token"), "registration_endpoint": serverEndpoint(request, "/register"),
		})
	}))
	defer server.Close()
	issuer = server.URL + "/tenant"
	metadata, err := discoverAuthorizationServer(context.Background(), issuer)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Issuer != issuer || metadata.AuthorizationEndpoint != server.URL+"/authorize" {
		t.Fatalf("authorization metadata = %#v", metadata)
	}
	issuer = server.URL + "/other"
	if _, err := discoverAuthorizationServer(context.Background(), server.URL+"/tenant"); err == nil {
		t.Fatal("metadata with a mismatched issuer was accepted")
	}
}

func structMCPHTTPConfig(endpoint string) projectconfig.MCPServerConfig {
	return projectconfig.MCPServerConfig{Transport: projectconfig.MCPTransportStreamableHTTP, URL: endpoint}
}

func serverEndpoint(request *http.Request, path string) string {
	return "http://" + request.Host + path
}
