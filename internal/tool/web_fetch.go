package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

const webFetchSchema = `{"type":"object","properties":{"url":{"type":"string","description":"Absolute HTTP or HTTPS URL to fetch"}},"required":["url"],"additionalProperties":false}`

// WebFetchTool fetches an HTML or text page and returns readable text.
type WebFetchTool struct {
	httpClient *http.Client
}

func (WebFetchTool) Definition() model.FunctionTool {
	return functionDefinition("web_fetch", "Fetch a web page and return its title and readable text.", webFetchSchema)
}

func (t WebFetchTool) Run(ctx context.Context, raw json.RawMessage) Result {
	var args struct {
		URL string `json:"url"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid web_fetch arguments: %w", err)}
	}
	target, err := parseFetchURL(args.URL)
	if err != nil {
		return Result{Err: err}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Result{Err: fmt.Errorf("create web_fetch request: %w", err)}
	}
	request.Header.Set("User-Agent", webUserAgent)
	request.Header.Set("Accept", "text/html, text/plain, application/json;q=0.9, */*;q=0.1")

	response, err := t.client().Do(request)
	if err != nil {
		return Result{Err: fmt.Errorf("fetch %q: %w", target.String(), err)}
	}
	defer response.Body.Close()
	body, err := readWebBody(response.Body)
	if err != nil {
		return Result{Err: fmt.Errorf("read %q: %w", target.String(), err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Result{Err: fmt.Errorf("fetch %q returned %s", target.String(), response.Status)}
	}

	mediaType, err := responseMediaType(response.Header.Get("Content-Type"), body)
	if err != nil {
		return Result{Err: fmt.Errorf("read content type: %w", err)}
	}
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		title, text, err := parseHTMLPage(body)
		if err != nil {
			return Result{Err: fmt.Errorf("parse HTML page: %w", err)}
		}
		text = limitWebText(text)
		if title != "" {
			return Result{Output: limitWebText("Title: "+title) + "\n\n" + text}
		}
		return Result{Output: text}
	case strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		return Result{Output: limitWebText(string(body))}
	default:
		return Result{Err: fmt.Errorf("unsupported content type %q; expected HTML, text, or JSON", mediaType)}
	}
}

func (t WebFetchTool) client() *http.Client {
	if t.httpClient != nil {
		return t.httpClient
	}
	return defaultWebHTTPClient
}

func parseFetchURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("url must not be empty")
	}
	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if !target.IsAbs() || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, errors.New("url must be an absolute HTTP or HTTPS URL")
	}
	if target.User != nil {
		return nil, errors.New("url must not contain embedded credentials")
	}
	return target, nil
}
