package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

func marshalStringOrParts(text *string, parts []ContentPart) ([]byte, error) {
	if text != nil && parts != nil {
		return nil, errors.New("content cannot contain both text and parts")
	}
	if text != nil {
		return json.Marshal(*text)
	}
	if parts != nil {
		return json.Marshal(parts)
	}
	return []byte("null"), nil
}

func unmarshalStringOrParts(data []byte) (*string, []ContentPart, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil, errors.New("content is empty")
	}
	if bytes.Equal(data, []byte("null")) {
		return nil, nil, nil
	}
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return nil, nil, err
		}
		return &text, nil, nil
	}
	var rawParts []json.RawMessage
	if err := json.Unmarshal(data, &rawParts); err != nil {
		return nil, nil, fmt.Errorf("decode content parts: %w", err)
	}
	parts := make([]ContentPart, 0, len(rawParts))
	for _, raw := range rawParts {
		part, err := decodeContentPart(raw)
		if err != nil {
			return nil, nil, err
		}
		parts = append(parts, part)
	}
	return nil, parts, nil
}

func marshalTagged(tag string, value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("encode %s item: %w", tag, err)
	}
	tagJSON, _ := json.Marshal(tag)
	object["type"] = tagJSON
	return json.Marshal(object)
}

func rawJSON(data json.RawMessage) []byte {
	if len(data) == 0 {
		return []byte("null")
	}
	return data
}

func cloneJSON(data []byte) json.RawMessage {
	return append(json.RawMessage(nil), data...)
}
