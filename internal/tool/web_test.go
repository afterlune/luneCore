package tool

import (
	"strings"
	"testing"
)

func TestReadWebBodyRejectsOversizedResponses(t *testing.T) {
	if _, err := readWebBody(strings.NewReader(strings.Repeat("x", maxWebResponse+1))); err == nil {
		t.Fatal("expected oversized response body to fail")
	}
}
