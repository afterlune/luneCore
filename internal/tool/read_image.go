package tool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
	publictool "github.com/afterlune/luneCore/tool"
)

const readImageSchema = `{"type":"object","properties":{"path":{"type":"string","description":"Workspace-relative image file, or an absolute file outside the workspace (which requires a session read grant)"}},"required":["path"],"additionalProperties":false}`

// ReadImageTool sends one bounded PNG, JPEG, or WebP image to the model.
type ReadImageTool struct {
	Workspace    string
	MaxFileBytes int64
	workspace    *workspaceIdentity
}

func (ReadImageTool) Definition() model.FunctionTool {
	return functionDefinition("read_image", "Read a PNG, JPEG, or WebP image and provide it to the model for visual analysis.", readImageSchema)
}

func (t ReadImageTool) Run(ctx context.Context, raw json.RawMessage) Result {
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("read_image stopped: %w", err)}
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid read_image arguments: %w", err)}
	}
	workspaceRoot, closeWorkspace, err := workspaceRootForTool(t.Workspace, t.workspace)
	if err != nil {
		return Result{Err: err}
	}
	defer closeWorkspace()
	resolved, err := resolveToolPath(workspaceRoot.Name(), args.Path, pathFile, true)
	if err != nil {
		return Result{Err: err}
	}
	root, closeRoot, relative, err := openResolvedToolPath(ctx, workspaceRoot, resolved, publictool.PathRead)
	if err != nil {
		return Result{Err: err}
	}
	defer closeRoot()
	file, err := root.Open(relative)
	if err != nil {
		return Result{Err: fmt.Errorf("open image %q: %w", args.Path, err)}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Result{Err: fmt.Errorf("stat image %q: %w", args.Path, err)}
	}
	if !info.Mode().IsRegular() {
		return Result{Err: errors.New("image path does not refer to a regular file")}
	}
	maximum := t.MaxFileBytes
	if maximum <= 0 {
		maximum = 10 << 20
	}
	if info.Size() > maximum {
		return Result{Err: fmt.Errorf("image exceeds the configured %d byte attachment limit", maximum)}
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return Result{Err: fmt.Errorf("read image %q: %w", args.Path, err)}
	}
	if int64(len(data)) > maximum {
		return Result{Err: fmt.Errorf("image exceeds the configured %d byte attachment limit", maximum)}
	}
	mime, err := imageMIME(data)
	if err != nil {
		return Result{Err: err}
	}
	if err := ctx.Err(); err != nil {
		return Result{Err: fmt.Errorf("read_image stopped: %w", err)}
	}
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
	message := fmt.Sprintf("Loaded %s image (%d bytes) from %s.", strings.TrimPrefix(mime, "image/"), len(data), args.Path)
	return Result{
		Output:  message,
		Details: &publictool.ExecutionDetails{Output: message},
		ModelOutput: func() *model.ToolOutput {
			output := model.NewToolOutputParts(model.InputText{Text: message}, model.InputImage{ImageURL: dataURL, Detail: "auto"})
			return &output
		}(),
	}
}

func imageMIME(data []byte) (string, error) {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png", nil
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg", nil
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp", nil
	default:
		return "", errors.New("unsupported image format; read_image accepts PNG, JPEG, and WebP")
	}
}

func (ReadImageTool) InstructionDirectories(ctx context.Context, r *Registry, args json.RawMessage) ([]string, bool, error) {
	return fileInstructionDirectoriesForPath(ctx, r, args, true)
}

