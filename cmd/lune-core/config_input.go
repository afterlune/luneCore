package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	core "github.com/afterlune/luneCore"
)

const maxStartupConfig = 1 << 20

type bufferedInput struct {
	*bufio.Reader
	io.Closer
}

// readStartupConfig consumes one bounded config frame and keeps any following
// bytes available to the JSON-RPC reader.
func readStartupConfig(input io.ReadCloser) (*core.ProjectConfig, io.ReadCloser, error) {
	reader := bufio.NewReaderSize(input, maxStartupConfig+1)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > maxStartupConfig {
		return nil, nil, errors.New("startup configuration must be one JSON line of at most 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var config *core.ProjectConfig
	if err := decoder.Decode(&config); err != nil || config == nil {
		return nil, nil, errors.New("startup configuration must match the Core project configuration schema")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, nil, errors.New("startup configuration must contain one JSON object")
	}
	if err := config.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid startup configuration: %w", err)
	}
	return config, &bufferedInput{Reader: reader, Closer: input}, nil
}
