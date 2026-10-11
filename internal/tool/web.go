package tool

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Shared settings for the web tools: one HTTP client, one response size cap,
// and one output length cap.
const (
	webRequestTimeout = 15 * time.Second
	maxWebResponse    = 2 << 20
	maxWebTextRunes   = 20_000
	webUserAgent      = "lune-agent"
)

var defaultWebHTTPClient = &http.Client{Timeout: webRequestTimeout}

func readWebBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxWebResponse+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxWebResponse {
		return nil, fmt.Errorf("response exceeds %d bytes", maxWebResponse)
	}
	return data, nil
}

func limitWebText(text string) string {
	text = strings.ToValidUTF8(text, "�")
	if utf8.RuneCountInString(text) <= maxWebTextRunes {
		return text
	}
	var limited strings.Builder
	count := 0
	for _, r := range text {
		if count == maxWebTextRunes {
			break
		}
		limited.WriteRune(r)
		count++
	}
	return limited.String() + "\n[content truncated at 20000 characters]"
}

func responseMediaType(header string, body []byte) (string, error) {
	if strings.TrimSpace(header) == "" {
		return strings.TrimSpace(strings.Split(http.DetectContentType(body), ";")[0]), nil
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return "", err
	}
	return strings.ToLower(mediaType), nil
}
