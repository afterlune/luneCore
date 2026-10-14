package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

func (remote *remoteTool) convertToolResult(ctx context.Context, raw json.RawMessage) (model.ToolOutput, []model.ContentPart, bool, error) {
	var response struct {
		ResultType        string            `json:"resultType"`
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
		IsError           bool              `json:"isError"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return model.ToolOutput{}, nil, false, fmt.Errorf("decode MCP tool result: %w", err)
	}
	parts := make([]model.ContentPart, 0)
	var transcript []model.ContentPart
	budget := binaryBudget{limits: remote.server.managerLimits()}
	if err := remote.convertContent(ctx, response.Content, 0, &budget, &parts, &transcript); err != nil {
		return model.ToolOutput{}, nil, false, err
	}
	if len(response.StructuredContent) > 0 && json.Valid(response.StructuredContent) {
		var formatted any
		if json.Unmarshal(response.StructuredContent, &formatted) == nil {
			data, _ := json.MarshalIndent(formatted, "", "  ")
			parts = append(parts, model.InputText{Text: "Structured content:\n" + string(data)})
		}
	}
	if len(parts) == 0 {
		parts = append(parts, model.InputText{Text: "MCP tool returned no model-visible content."})
	}
	return model.NewToolOutputParts(parts...), transcript, response.IsError, nil
}

func (instance *serverInstance) managerLimits() Limits {
	if instance == nil || instance.owner == nil {
		return Limits{MaxBinaryBlocks: 5, MaxFileBytes: 10 << 20, MaxTotalBytes: 20 << 20}
	}
	return instance.owner.limits
}

func (remote *remoteTool) convertContent(ctx context.Context, blocks []json.RawMessage, depth int, budget *binaryBudget, parts *[]model.ContentPart, transcript *[]model.ContentPart) error {
	for _, raw := range blocks {
		var block struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Data        string `json:"data"`
			MIMEType    string `json:"mimeType"`
			URI         string `json:"uri"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Annotations struct {
				Audience []string `json:"audience"`
			} `json:"annotations"`
			Resource *struct {
				URI      string `json:"uri"`
				MIMEType string `json:"mimeType"`
				Text     string `json:"text"`
				Blob     string `json:"blob"`
			} `json:"resource"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			return fmt.Errorf("decode MCP content block: %w", err)
		}
		userOnly := hasAudience(block.Annotations.Audience, "user") && !hasAudience(block.Annotations.Audience, "assistant")
		switch block.Type {
		case "text":
			if userOnly {
				*transcript = append(*transcript, model.InputText{Text: block.Text})
			} else {
				*parts = append(*parts, model.InputText{Text: block.Text})
			}
		case "image", "audio", "resource":
			if block.Type == "resource" && block.Resource != nil {
				if block.Resource.Text != "" {
					if userOnly {
						*transcript = append(*transcript, model.InputText{Text: block.Resource.Text})
					} else {
						*parts = append(*parts, model.InputText{Text: block.Resource.Text})
					}
				}
				if block.Resource.Blob != "" {
					if err := remote.addBinary(block.Resource.Blob, block.Resource.MIMEType, userOnly, ctx, budget, parts, transcript); err != nil {
						return err
					}
				}
				continue
			}
			if err := remote.addBinary(block.Data, block.MIMEType, userOnly, ctx, budget, parts, transcript); err != nil {
				return err
			}
		case "resource_link":
			if block.URI == "" {
				return errors.New("MCP resource link did not include a URI")
			}
			if depth > 0 {
				marker := fmt.Sprintf("[Resource link: %s (%s)]", block.Name, block.URI)
				if userOnly {
					*transcript = append(*transcript, model.InputText{Text: marker})
				} else {
					*parts = append(*parts, model.InputText{Text: marker})
				}
				continue
			}
			params := map[string]any{"uri": block.URI}
			resource, err := remote.server.client.Request(ctx, "resources/read", params, block.URI, nil)
			if err != nil {
				return fmt.Errorf("read MCP resource %q: %w", block.URI, err)
			}
			resource, err = remote.withMRTR(ctx, "resources/read", params, block.URI, nil, resource)
			if err != nil {
				return fmt.Errorf("read MCP resource %q: %w", block.URI, err)
			}
			var read struct {
				Contents []json.RawMessage `json:"contents"`
			}
			if err := json.Unmarshal(resource, &read); err != nil {
				return fmt.Errorf("decode MCP resource %q: %w", block.URI, err)
			}
			if err := remote.convertReadContents(ctx, read.Contents, userOnly, budget, parts, transcript); err != nil {
				return err
			}
		default:
			marker := fmt.Sprintf("[Unsupported MCP content type: %s]", block.Type)
			if userOnly {
				*transcript = append(*transcript, model.InputText{Text: marker})
			} else {
				*parts = append(*parts, model.InputText{Text: marker})
			}
		}
	}
	return nil
}

func (remote *remoteTool) convertReadContents(ctx context.Context, items []json.RawMessage, userOnly bool, budget *binaryBudget, parts *[]model.ContentPart, transcript *[]model.ContentPart) error {
	for _, raw := range items {
		var resource struct {
			MIMEType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		}
		if err := json.Unmarshal(raw, &resource); err != nil {
			return fmt.Errorf("decode MCP resource content: %w", err)
		}
		if resource.Text != "" {
			if userOnly {
				*transcript = append(*transcript, model.InputText{Text: resource.Text})
			} else {
				*parts = append(*parts, model.InputText{Text: resource.Text})
			}
		}
		if resource.Blob != "" {
			if err := remote.addBinary(resource.Blob, resource.MIMEType, userOnly, ctx, budget, parts, transcript); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasAudience(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type binaryBudget struct {
	limits Limits
	blocks int
	total  int64
}

func (remote *remoteTool) addBinary(data, mimeType string, userOnly bool, ctx context.Context, budget *binaryBudget, parts *[]model.ContentPart, transcript *[]model.ContentPart) error {
	decoded, err := jsonBase64(data)
	if err != nil {
		return fmt.Errorf("decode MCP binary content: %w", err)
	}
	budget.blocks++
	budget.total += int64(len(decoded))
	if budget.blocks > budget.limits.MaxBinaryBlocks {
		return fmt.Errorf("MCP result exceeds the %d binary block limit", budget.limits.MaxBinaryBlocks)
	}
	if int64(len(decoded)) > budget.limits.MaxFileBytes {
		return fmt.Errorf("MCP binary block exceeds the %d byte limit", budget.limits.MaxFileBytes)
	}
	if budget.total > budget.limits.MaxTotalBytes {
		return fmt.Errorf("MCP result exceeds the %d byte aggregate limit", budget.limits.MaxTotalBytes)
	}
	if mediaType, _, err := mime.ParseMediaType(mimeType); err == nil {
		mimeType = strings.ToLower(mediaType)
	} else {
		mimeType = "application/octet-stream"
	}
	if userOnly {
		*transcript = append(*transcript, model.InputText{Text: binaryMarker(mimeType, len(decoded))})
		if strings.HasPrefix(mimeType, "image/") {
			*transcript = append(*transcript, model.InputImage{ImageURL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(decoded)})
		} else if strings.HasPrefix(mimeType, "audio/") {
			if format := audioFormat(mimeType); format != "" {
				*transcript = append(*transcript, model.InputAudio{Data: base64.StdEncoding.EncodeToString(decoded), Format: format})
			}
		}
		return nil
	}
	if strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		encoded := base64.StdEncoding.EncodeToString(decoded)
		*parts = append(*parts, model.InputImage{ImageURL: "data:" + mimeType + ";base64," + encoded})
		return nil
	}
	if strings.HasPrefix(strings.ToLower(mimeType), "audio/") {
		if model.InputAudioSupported(ctx) {
			format := audioFormat(mimeType)
			if format != "" {
				*parts = append(*parts, model.InputAudio{Data: base64.StdEncoding.EncodeToString(decoded), Format: format})
			} else {
				*parts = append(*parts, model.InputText{Text: binaryMarker(mimeType, len(decoded)) + " (audio format is not supported by this model input type)"})
			}
		} else {
			*parts = append(*parts, model.InputText{Text: binaryMarker(mimeType, len(decoded)) + " (the active model profile does not support audio input)"})
		}
		return nil
	}
	*parts = append(*parts, model.InputText{Text: binaryMarker(mimeType, len(decoded))})
	return nil
}

func jsonBase64(value string) ([]byte, error) {
	if result, err := base64.StdEncoding.DecodeString(value); err == nil {
		return result, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}

func binaryMarker(mimeType string, size int) string {
	return fmt.Sprintf("[Binary content: %s, %s]", mimeType, byteSize(int64(size)))
}

func byteSize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	return fmt.Sprintf("%.1f KiB", float64(size)/1024)
}

func audioFormat(mimeType string) string {
	base := strings.ToLower(strings.SplitN(mimeType, ";", 2)[0])
	switch base {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return "wav"
	default:
		return ""
	}
}
