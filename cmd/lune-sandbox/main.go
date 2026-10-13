package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/afterlune/luneCore/internal/sandbox"
	publictool "github.com/afterlune/luneCore/tool"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: lune-sandbox run")
		os.Exit(2)
	}
	var request publictool.CommandRequest
	input := bufio.NewReader(os.Stdin)
	line, err := input.ReadBytes('\n')
	if err != nil {
		fmt.Fprintln(os.Stderr, "read sandbox request:", err)
		os.Exit(2)
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		fmt.Fprintln(os.Stderr, "decode sandbox request:", err)
		os.Exit(2)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if runtime.GOOS == "windows" {
		go func() {
			_, _ = io.Copy(io.Discard, input)
			cancel()
		}()
	}
	if err := sandbox.RunContext(ctx, request, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
