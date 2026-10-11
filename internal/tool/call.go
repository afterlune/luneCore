package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/afterlune/luneCore/internal/model"
)

// Tool is an executable function tool exposed through the Responses API.
type Tool interface {
	Definition() model.FunctionTool
	Run(context.Context, json.RawMessage) Result
}

func decodeArguments(data json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("tool arguments are empty")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("tool arguments contain multiple JSON values")
		}
		return err
	}
	return nil
}
