package mcp

import (
	"context"
	"os/exec"
	"runtime"
)

// BrowserOpener controls how interactive authorization URLs are presented to the user.
type BrowserOpener interface {
	Open(ctx context.Context, target string) error
}

// SystemBrowserOpener launches the operating system default browser.
type SystemBrowserOpener struct{}

func (SystemBrowserOpener) Open(ctx context.Context, target string) error {
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
	process := exec.CommandContext(ctx, command, args...)
	if err := process.Start(); err != nil {
		return err
	}
	go func() { _ = process.Wait() }()
	return nil
}

// HeadlessBrowserOpener is an opener for headless/server environments that records or prints the URL.
type HeadlessBrowserOpener struct {
	OnOpen func(target string)
}

func (h HeadlessBrowserOpener) Open(_ context.Context, target string) error {
	if h.OnOpen != nil {
		h.OnOpen(target)
	}
	return nil
}
