// Package runtimehost lets an application expose the Core Runtime Protocol through its own HTTP server.
package runtimehost

import (
	"errors"
	"strings"

	lunecore "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/internal/rpcserver"
)

// Server handles authenticated Runtime Protocol WebSocket upgrades at /v1/runtime.
// The host supplies the listener, transport security, and any additional origin checks.
type Server = rpcserver.Server

// New creates a host for a single Core. A bearer token is required for every connection.
func New(client *lunecore.Core, token string) (*Server, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("runtime host requires an access token")
	}
	return rpcserver.New(client, token)
}
