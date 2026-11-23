package rpcserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/afterlune/luneCore/model"
)

const (
	maxAttachmentFiles      = 5
	maxAttachmentFileBytes  = 2 << 20
	maxAttachmentTotalBytes = 4 << 20
	maxAttachmentTextBytes  = 128 << 10
)

type attachment struct {
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Data      []byte `json:"data"`
}

func attachmentInput(params submitParams) (json.RawMessage, error) {
	if len(params.Attachments) > maxAttachmentFiles {
		return nil, fmt.Errorf("a message can contain up to %d attachments", maxAttachmentFiles)
	}
	var prompt string
	if err := json.Unmarshal(params.Input, &prompt); err != nil || len(prompt) > maxAttachmentTextBytes {
		return nil, fmt.Errorf("messages with attachments require a plain text prompt of at most 128 KiB")
	}
	parts := make([]model.ContentPart, 0, 1+len(params.Attachments)*2)
	if prompt != "" {
		parts = append(parts, model.InputText{Text: prompt})
	}
	total := 0
	for _, file := range params.Attachments {
		if file.Name == "" || len(file.Name) > 255 || file.Name == "." || file.Name == ".." || strings.ContainsAny(file.Name, "/\\") || strings.ContainsFunc(file.Name, unicode.IsControl) {
			return nil, fmt.Errorf("invalid attachment filename")
		}
		total += len(file.Data)
		if len(file.Data) > maxAttachmentFileBytes || total > maxAttachmentTotalBytes {
			return nil, fmt.Errorf("attachments are limited to 2 MiB per file and 4 MiB per message")
		}
		name, _ := json.Marshal(file.Name)
		label := "Attachment: " + string(name)
		switch file.MediaType {
		case "text/plain", "text/markdown", "text/csv":
			if len(file.Data) > maxAttachmentTextBytes || !utf8.Valid(file.Data) || bytes.ContainsRune(file.Data, 0) {
				return nil, fmt.Errorf("%s must be UTF-8 text of at most 128 KiB", file.Name)
			}
			if ext := strings.ToLower(path.Ext(file.Name)); ext != ".txt" && ext != ".md" && ext != ".markdown" && ext != ".csv" && ext != "" {
				return nil, fmt.Errorf("unsupported text attachment: %s", file.Name)
			}
			parts = append(parts, model.InputText{Text: label + "\n" + string(file.Data)})
		case "image/png", "image/jpeg":
			config, format, err := image.DecodeConfig(bytes.NewReader(file.Data))
			if err != nil || "image/"+format != file.MediaType || config.Width <= 0 || config.Height <= 0 || config.Width > 8192 || config.Height > 8192 {
				return nil, fmt.Errorf("%s must be a PNG or JPEG image with dimensions up to 8192 pixels", file.Name)
			}
			parts = append(parts, model.InputText{Text: label}, model.InputImage{
				ImageURL: "data:" + file.MediaType + ";base64," + base64.StdEncoding.EncodeToString(file.Data),
			})
		default:
			return nil, fmt.Errorf("unsupported attachment format: %s", file.MediaType)
		}
	}
	return json.Marshal(model.NewResponseInputItems(model.InputMessage{
		Role: model.RoleUser, Content: model.NewMessageContentParts(parts...),
	}))
}

func (c *connection) historyPage(raw json.RawMessage) (any, *rpcError) {
	var params struct {
		SessionID string `json:"sessionId"`
		Offset    int    `json:"offset"`
		Digest    string `json:"digest"`
	}
	if err := c.readParams(raw, &params); err != nil {
		return nil, err
	}
	saved, err := c.server.core.GetSession(c.ctx, params.SessionID)
	if err != nil {
		return nil, applicationError(err)
	}
	digest := historyDigest(saved.History)
	if params.Digest != "" && params.Digest != digest {
		return nil, &rpcError{Code: -32001, Message: "session history changed while loading", Data: "history_changed"}
	}
	var items []json.RawMessage
	if len(saved.History) > 0 {
		if err := json.Unmarshal(saved.History, &items); err != nil {
			var wrapped struct {
				Items []json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(saved.History, &wrapped); err != nil {
				return nil, applicationError(err)
			}
			items = wrapped.Items
		}
	}
	if params.Offset < 0 || params.Offset > len(items) {
		return nil, &rpcError{Code: -32602, Message: "invalid history offset"}
	}
	end, size := params.Offset, 0
	for end < len(items) && end-params.Offset < 100 {
		itemSize := len(items[end])
		if itemSize > MaxMessageBytes-4096 {
			return nil, &rpcError{Code: -32001, Message: "a history item exceeds the maximum response size"}
		}
		if end > params.Offset && size+itemSize > 6<<20 {
			break
		}
		size += itemSize
		end++
	}
	return map[string]any{"items": items[params.Offset:end], "nextOffset": end, "done": end == len(items), "digest": digest}, nil
}
