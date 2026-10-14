//go:build !windows && !(linux && (amd64 || arm64))

package sandbox

import (
	"context"
	"errors"
	"io"

	publictool "github.com/afterlune/luneCore/tool"
)

func runPlatform(context.Context, publictool.CommandRequest, io.Writer) error {
	return errors.New("kernel command sandbox is supported only on Windows and Linux")
}

func runChild(publictool.CommandRequest) error {
	return errors.New("sandbox child mode is supported only on Linux")
}
