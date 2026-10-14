package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"

	publicmodel "github.com/afterlune/luneCore/model"
)

const (
	mcpFixturePromptName        = "visual-reference"
	mcpFixturePromptText        = "Briefly describe the picture and use the reference below."
	mcpFixtureAssistantText     = "I will check the picture against the reference."
	mcpFixtureResourceURI       = "urn:smoke-reference"
	mcpFixtureResourceText      = "Reference: a small blue square."
	mcpFixturePromptDescription = "Visual reference prompt"
)

func newMCPPromptResourceFixture(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	var imageBuffer bytes.Buffer
	fixtureImage := image.NewRGBA(image.Rect(0, 0, 1, 1))
	fixtureImage.Set(0, 0, color.RGBA{R: 24, G: 128, B: 220, A: 255})
	if err := png.Encode(&imageBuffer, fixtureImage); err != nil {
		t.Fatal(err)
	}
	imageData := base64.StdEncoding.EncodeToString(imageBuffer.Bytes())
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var message hostMCPRequest
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode MCP request: %v", err)
			return
		}
		var result any
		switch message.Method {
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		case "prompts/get":
			var name string
			if err := json.Unmarshal(message.Params["name"], &name); err != nil || name != mcpFixturePromptName {
				t.Errorf("prompt name = %q, error = %v", name, err)
			}
			result = map[string]any{"description": mcpFixturePromptDescription, "messages": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": mcpFixturePromptText},
					map[string]any{"type": "image", "mimeType": "image/png", "data": imageData},
				}},
				map[string]any{"role": "assistant", "content": map[string]any{"type": "text", "text": mcpFixtureAssistantText}},
			}}
		case "resources/read":
			var uri string
			if err := json.Unmarshal(message.Params["uri"], &uri); err != nil || uri != mcpFixtureResourceURI {
				t.Errorf("resource URI = %q, error = %v", uri, err)
			}
			result = map[string]any{"contents": []any{
				map[string]any{"uri": mcpFixtureResourceURI, "mimeType": "text/plain", "text": mcpFixtureResourceText},
			}}
		default:
			t.Errorf("unexpected MCP method %q", message.Method)
			return
		}
		writeHostMCPResult(writer, message.ID, result)
	}))
	t.Cleanup(server.Close)
	return server, "data:image/png;base64," + imageData
}

func assertMCPPromptResourceInput(t *testing.T, input *publicmodel.ResponseInput, expectedImageURL string) {
	t.Helper()
	if input == nil || input.Text != nil || len(input.Items) < 3 {
		t.Fatalf("model input = %#v, want at least three ordered MCP messages", input)
	}
	userPrompt, ok := input.Items[0].(publicmodel.InputMessage)
	if !ok || userPrompt.Role != publicmodel.RoleUser || len(userPrompt.Content.Parts) != 2 {
		t.Fatalf("prompt user message = %#v", input.Items[0])
	}
	if text, ok := userPrompt.Content.Parts[0].(publicmodel.InputText); !ok || text.Text != mcpFixturePromptText {
		t.Fatalf("prompt text = %#v", userPrompt.Content.Parts[0])
	}
	if image, ok := userPrompt.Content.Parts[1].(publicmodel.InputImage); !ok || image.ImageURL != expectedImageURL {
		t.Fatalf("prompt image = %#v", userPrompt.Content.Parts[1])
	}
	assistantPrompt, ok := input.Items[1].(publicmodel.InputMessage)
	if !ok || assistantPrompt.Role != publicmodel.RoleAssistant || len(assistantPrompt.Content.Parts) != 1 {
		t.Fatalf("prompt assistant message = %#v", input.Items[1])
	}
	if text, ok := assistantPrompt.Content.Parts[0].(publicmodel.OutputText); !ok || text.Text != mcpFixtureAssistantText {
		t.Fatalf("assistant prompt text = %#v", assistantPrompt.Content.Parts[0])
	}
	resource, ok := input.Items[2].(publicmodel.InputMessage)
	if !ok || resource.Role != publicmodel.RoleUser || len(resource.Content.Parts) != 1 {
		t.Fatalf("resource message = %#v", input.Items[2])
	}
	if text, ok := resource.Content.Parts[0].(publicmodel.InputText); !ok || text.Text != mcpFixtureResourceText {
		t.Fatalf("resource text = %#v", resource.Content.Parts[0])
	}
}
