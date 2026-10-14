package mcp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"
)

const (
	oauthCallbackPath = "/oauth/callback"
	oauthCIMDPort     = "42477"
)

type oauthCallback struct {
	Code   string
	State  string
	Issuer string
	Error  string
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
