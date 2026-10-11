package model

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func postJSON(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, body any) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode model request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(encoded)))
	if err != nil {
		return nil, fmt.Errorf("create model request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call model provider: %w", err)
	}
	return response, nil
}

func responseBody(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read model response: %w", err)
	}
	if len(body) > maxResponseBodyBytes {
		return nil, fmt.Errorf("model response exceeds %d bytes", maxResponseBodyBytes)
	}
	return body, nil
}

func httpStatusError(response *http.Response, body []byte, provider string) error {
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = response.Status
	}
	return fmt.Errorf("%s returned %s: %s", provider, response.Status, message)
}

func scanSSE(ctx context.Context, body io.Reader, handle func(event, data string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), maxResponseBodyBytes)
	eventName := ""
	var dataLines []string
	dispatch := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		if err := handle(eventName, strings.Join(dataLines, "\n")); err != nil {
			return err
		}
		eventName = ""
		dataLines = dataLines[:0]
		return nil
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			eventName = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read model event stream: %w", err)
	}
	return dispatch()
}

func decodeSSEJSON(data string, value any) error {
	if strings.TrimSpace(data) == "[DONE]" {
		return errors.New("unexpected [DONE] marker in semantic event stream")
	}
	if err := json.Unmarshal([]byte(data), value); err != nil {
		return fmt.Errorf("decode model stream event: %w", err)
	}
	return nil
}
