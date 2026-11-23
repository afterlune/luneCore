package main

import (
	"io"
	"strings"
	"testing"
)

func TestReadStartupConfigPreservesRuntimeFrames(t *testing.T) {
	const frame = `{"models":{"providers":{"fixture":{"protocol":"responses","baseURL":"http://127.0.0.1","apiKey":"fixture"}},"profiles":{"fixture":{"provider":"fixture","model":"fixture-model"}},"defaultProfile":"fixture"}}`
	const rpc = "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"runtime.hello\"}\n"
	cfg, input, err := readStartupConfig(io.NopCloser(strings.NewReader(frame + "\n" + rpc)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.DefaultProfile != "fixture" {
		t.Fatalf("unexpected configuration: %s", cfg.Models.DefaultProfile)
	}
	rest, err := io.ReadAll(input)
	if err != nil || string(rest) != rpc {
		t.Fatalf("following frame = %q, error = %v", rest, err)
	}

	for _, bad := range []string{"null\n", "{}\n", frame, frame + "{}\n", strings.Repeat("x", maxStartupConfig) + "\n"} {
		if _, _, err := readStartupConfig(io.NopCloser(strings.NewReader(bad))); err == nil {
			t.Fatalf("accepted invalid startup configuration with %d bytes", len(bad))
		}
	}
}
