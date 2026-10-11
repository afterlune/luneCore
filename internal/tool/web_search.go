package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/afterlune/luneCore/internal/model"
	"golang.org/x/net/html"
)

const (
	duckDuckGoLiteURL = "https://lite.duckduckgo.com/lite/"
	webSearchSchema   = `{"type":"object","properties":{"query":{"type":"string","description":"Search query"},"max_results":{"type":"integer","minimum":1,"maximum":10,"description":"Number of results to return (1-10)"}},"required":["query","max_results"],"additionalProperties":false}`
)

// WebSearchTool searches DuckDuckGo Lite and returns a JSON array of results.
type WebSearchTool struct {
	httpClient *http.Client
	endpoint   string
}

func (WebSearchTool) Definition() model.FunctionTool {
	return functionDefinition("web_search", "Search the web with DuckDuckGo and return result titles, URLs, and snippets.", webSearchSchema)
}

func (t WebSearchTool) Run(ctx context.Context, raw json.RawMessage) Result {
	var args struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := decodeArguments(raw, &args); err != nil {
		return Result{Err: fmt.Errorf("invalid web_search arguments: %w", err)}
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return Result{Err: errors.New("query must not be empty")}
	}
	if args.MaxResults < 1 || args.MaxResults > 10 {
		return Result{Err: errors.New("max_results must be between 1 and 10")}
	}

	endpoint := t.endpoint
	if endpoint == "" {
		endpoint = duckDuckGoLiteURL
	}
	searchURL, err := url.Parse(endpoint)
	if err != nil || !searchURL.IsAbs() || searchURL.Host == "" || (searchURL.Scheme != "http" && searchURL.Scheme != "https") {
		return Result{Err: errors.New("invalid DuckDuckGo search endpoint")}
	}
	query := searchURL.Query()
	query.Set("q", args.Query)
	searchURL.RawQuery = query.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL.String(), nil)
	if err != nil {
		return Result{Err: fmt.Errorf("create web_search request: %w", err)}
	}
	request.Header.Set("User-Agent", webUserAgent)
	request.Header.Set("Accept", "text/html")

	response, err := t.client().Do(request)
	if err != nil {
		return Result{Err: fmt.Errorf("search DuckDuckGo: %w", err)}
	}
	defer response.Body.Close()
	body, err := readWebBody(response.Body)
	if err != nil {
		return Result{Err: fmt.Errorf("read DuckDuckGo response: %w", err)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Result{Err: fmt.Errorf("DuckDuckGo returned %s", response.Status)}
	}

	results, err := parseDuckDuckGoResults(body, args.MaxResults)
	if err != nil {
		return Result{Err: fmt.Errorf("parse DuckDuckGo results: %w", err)}
	}
	encoded, err := json.Marshal(results)
	if err != nil {
		return Result{Err: fmt.Errorf("encode web_search results: %w", err)}
	}
	return Result{Output: string(encoded)}
}

func (t WebSearchTool) client() *http.Client {
	if t.httpClient != nil {
		return t.httpClient
	}
	return defaultWebHTTPClient
}

type webSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

func parseDuckDuckGoResults(body []byte, maxResults int) ([]webSearchResult, error) {
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	rows := make([]*html.Node, 0)
	collectElements(document, "tr", &rows)

	results := make([]webSearchResult, 0, maxResults)
	for i, row := range rows {
		anchor := findClassElement(row, "a", "result-link")
		if anchor == nil {
			continue
		}
		href := attribute(anchor, "href")
		resultURL, err := unwrapDuckDuckGoURL(href)
		if err != nil {
			continue
		}
		title := nodeText(anchor)
		if title == "" {
			continue
		}

		result := webSearchResult{Title: title, URL: resultURL}
		for j := i + 1; j < len(rows); j++ {
			if findClassElement(rows[j], "a", "result-link") != nil {
				break
			}
			if snippet := findClassElement(rows[j], "", "result-snippet"); snippet != nil {
				result.Snippet = nodeText(snippet)
				break
			}
		}
		results = append(results, result)
		if len(results) == maxResults {
			break
		}
	}
	return results, nil
}

func unwrapDuckDuckGoURL(href string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return "", err
	}
	host := strings.ToLower(parsed.Hostname())
	if (host == "duckduckgo.com" || strings.HasSuffix(host, ".duckduckgo.com")) && strings.HasPrefix(parsed.Path, "/l/") {
		if target := parsed.Query().Get("uddg"); target != "" {
			parsed, err = url.Parse(target)
			if err != nil {
				return "", err
			}
		}
	}
	if !parsed.IsAbs() {
		base, _ := url.Parse("https://duckduckgo.com/")
		parsed = base.ResolveReference(parsed)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("result URL is not HTTP or HTTPS")
	}
	return parsed.String(), nil
}
