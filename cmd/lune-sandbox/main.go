package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/afterlune/luneCore/internal/sandbox"
)

func main() {
	input := bufio.NewReader(os.Stdin)
	if len(os.Args) == 2 && os.Args[1] == "exec" {
		wire, err := readRequest(input)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read sandbox child request:", err)
			os.Exit(2)
		}
		if err := sandbox.RunChild(wire.CommandRequest); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: lune-sandbox run")
		os.Exit(2)
	}
	wire, err := readRequest(input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read sandbox request:", err)
		os.Exit(2)
	}
	request := wire.CommandRequest
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" {
		go func() {
			_, _ = io.Copy(io.Discard, input)
			cancel()
		}()
	}
	if err := sandbox.RunContext(ctx, request, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var exit *sandbox.ExitError
		if errors.As(err, &exit) && exit.Code > 0 {
			os.Exit(exit.Code)
		}
		os.Exit(1)
	}
}

func readRequest(input *bufio.Reader) (sandbox.HelperRequest, error) {
	var wire sandbox.HelperRequest
	line, err := input.ReadBytes('\n')
	if err != nil {
		return wire, err
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return wire, err
	}
	if wire.ProtocolVersion != sandbox.HelperProtocolVersion {
		return wire, errors.New("unsupported sandbox helper protocol; rebuild lune-sandbox with Core")
	}
	return wire, nil
}
