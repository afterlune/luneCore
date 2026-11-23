package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	core "github.com/afterlune/luneCore"
	"github.com/afterlune/luneCore/internal/rpcserver"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("lune-core stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lune-core <stdio|serve> [options]")
	}
	mode := args[0]
	flags := flag.NewFlagSet("lune-core "+mode, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	workspace := flags.String("workspace", "", "workspace opened by Core")
	stateDir := flags.String("state-dir", "", "Core state directory")
	helperPath := flags.String("sandbox-helper", "", "path to lune-sandbox helper")
	listen := flags.String("listen", "127.0.0.1:18092", "HTTPS listener address for WSS clients")
	tlsCert := flags.String("tls-cert", "", "TLS certificate PEM file")
	tlsKey := flags.String("tls-key", "", "TLS private key PEM file")
	tokenFile := flags.String("token-file", "", "file containing the remote Bearer token")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if mode != "stdio" && mode != "serve" {
		return fmt.Errorf("unknown mode %q, expected stdio or serve", mode)
	}
	if mode == "stdio" && (flags.Lookup("listen").Value.String() != "127.0.0.1:18092" || *tlsCert != "" || *tlsKey != "" || *tokenFile != "") {
		return errors.New("serve options cannot be used with stdio")
	}

	var token string
	if mode == "serve" {
		if *tlsCert == "" || *tlsKey == "" || *tokenFile == "" {
			return errors.New("serve requires --tls-cert, --tls-key, and --token-file")
		}
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("read token file: %w", err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return errors.New("token file is empty")
		}
	}

	client, err := core.New(core.Config{
		Workspace: *workspace, StateDir: *stateDir, SandboxHelperPath: *helperPath,
		ExclusiveState: true,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	service, err := rpcserver.New(client, token)
	if err != nil {
		_ = client.Close(context.Background())
		return err
	}
	var serveErr error
	if mode == "stdio" {
		serveErr = service.ServeStdio(ctx, os.Stdin, os.Stdout)
	} else {
		serveErr = serveHTTPS(ctx, service, *listen, *tlsCert, *tlsKey)
		service.Shutdown()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	interruptSessions(shutdownCtx, client)
	closeErr := client.Close(shutdownCtx)
	return errors.Join(serveErr, closeErr)
}

func serveHTTPS(ctx context.Context, service *rpcserver.Server, address, certificate, key string) error {
	server := &http.Server{
		Addr: address, Handler: service,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	result := make(chan error, 1)
	go func() {
		err := server.ListenAndServeTLS(certificate, key)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		result <- err
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		service.Shutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		serveErr := <-result
		return errors.Join(shutdownErr, serveErr)
	}
}

func interruptSessions(ctx context.Context, client *core.Core) {
	const pageSize = 500
	for offset := 0; ; offset += pageSize {
		sessions, err := client.ListSessionPage(ctx, offset, pageSize, true)
		if err != nil || len(sessions) == 0 {
			return
		}
		for _, saved := range sessions {
			runtime, err := client.OpenSessionRuntime(ctx, saved.ID)
			if err != nil {
				continue
			}
			_, _ = runtime.CancelPendingTurns(ctx)
			_ = runtime.Close(ctx)
		}
		if len(sessions) < pageSize {
			return
		}
	}
}
