package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
)

func TestValidateTaskInputAttachmentLimits(t *testing.T) {
	dataURL := func(value string) string {
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(value))
	}
	tests := []struct {
		name    string
		images  []model.InputImage
		limits  projectconfig.AttachmentLimits
		wantErr string
	}{
		{
			name:   "within per-file and total limits",
			images: []model.InputImage{{ImageURL: dataURL("1234")}, {ImageURL: dataURL("567")}},
			limits: projectconfig.AttachmentLimits{MaxFiles: 2, MaxFileBytes: 4, MaxTotalBytes: 7},
		},
		{
			name:   "case-insensitive data URL header",
			images: []model.InputImage{{ImageURL: "DATA:IMAGE/PNG;BASE64,MTIz"}},
			limits: projectconfig.AttachmentLimits{MaxFileBytes: 3},
		},
		{
			name:   "URL-escaped base64 payload",
			images: []model.InputImage{{ImageURL: "data:image/png;base64,%2B%2F8%3D"}},
			limits: projectconfig.AttachmentLimits{MaxFileBytes: 2},
		},
		{
			name:    "malformed URL escape",
			images:  []model.InputImage{{ImageURL: "data:image/png;base64,%2G%2F8%3D"}},
			wantErr: "valid base64",
		},
		{
			name:    "malformed URL escape after base64 padding",
			images:  []model.InputImage{{ImageURL: "data:image/png;base64,YQ==%2G"}},
			wantErr: "valid base64",
		},
		{
			name:    "too many images",
			images:  []model.InputImage{{ImageURL: dataURL("1")}, {ImageURL: dataURL("2")}},
			limits:  projectconfig.AttachmentLimits{MaxFiles: 1},
			wantErr: "attachments.maxFiles",
		},
		{
			name: "default image count limit",
			images: []model.InputImage{
				{ImageURL: dataURL("1")}, {ImageURL: dataURL("2")}, {ImageURL: dataURL("3")},
				{ImageURL: dataURL("4")}, {ImageURL: dataURL("5")}, {ImageURL: dataURL("6")},
			},
			wantErr: "attachments.maxFiles",
		},
		{
			name:    "file byte limit",
			images:  []model.InputImage{{ImageURL: dataURL("12345")}},
			limits:  projectconfig.AttachmentLimits{MaxFileBytes: 4},
			wantErr: "attachments.maxFileBytes",
		},
		{
			name:    "total byte limit",
			images:  []model.InputImage{{ImageURL: dataURL("1234")}, {ImageURL: dataURL("5678")}},
			limits:  projectconfig.AttachmentLimits{MaxTotalBytes: 7},
			wantErr: "attachments.maxTotalBytes",
		},
		{
			name:    "malformed base64",
			images:  []model.InputImage{{ImageURL: "data:image/png;base64,not base64!"}},
			wantErr: "valid base64",
		},
		{
			name:    "remote image cannot satisfy byte limits",
			images:  []model.InputImage{{ImageURL: "https://example.test/image.png"}},
			limits:  projectconfig.AttachmentLimits{MaxTotalBytes: 1024},
			wantErr: "size cannot be verified",
		},
		{
			name:   "remote image with count-only limit",
			images: []model.InputImage{{FileID: "file-1"}},
			limits: projectconfig.AttachmentLimits{MaxFiles: 1},
		},
		{
			name:   "remote image remains allowed under defaults",
			images: []model.InputImage{{ImageURL: "https://example.test/image.png"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := taskInputWithImages(test.images...)
			err := validateTaskInputAttachments(input, test.limits)
			if test.wantErr == "" && err != nil {
				t.Fatalf("validateTaskInputAttachments: %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("validation error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestRunTaskRejectsAttachmentsBeforeStartingWhenOverLimit(t *testing.T) {
	input, err := json.Marshal(taskInputWithImages(
		model.InputImage{ImageURL: "data:image/png;base64,YQ=="},
		model.InputImage{ImageURL: "data:image/png;base64,Yg=="},
	))
	if err != nil {
		t.Fatal(err)
	}
	client := &Core{attachmentLimits: projectconfig.AttachmentLimits{MaxFiles: 1}, active: TaskState{Status: TaskIdle}}
	_, err = client.RunTask(context.Background(), TaskRequest{SessionID: "not-started", Input: TaskInput{Raw: input}})
	if err == nil || !strings.Contains(err.Error(), "attachments.maxFiles") {
		t.Fatalf("RunTask validation error = %v, want attachment limit error", err)
	}
	if state := client.State(); state.Status != TaskIdle || state.TaskID != "" {
		t.Fatalf("task started before attachment validation: %#v", state)
	}
}

func TestProjectAttachmentLimitsExposeEffectiveProjectValues(t *testing.T) {
	defaults := (&Core{}).ProjectAttachmentLimits()
	wantDefaults := AttachmentLimits{
		MaxFiles: DefaultAttachmentMaxFiles, MaxFileBytes: DefaultAttachmentMaxFileBytes,
		MaxTotalBytes: DefaultAttachmentMaxTotalBytes,
	}
	if defaults != wantDefaults {
		t.Fatalf("default ProjectAttachmentLimits() = %#v, want %#v", defaults, wantDefaults)
	}
	client := &Core{attachmentLimits: projectconfig.AttachmentLimits{
		MaxFiles: 3, MaxFileBytes: 4 << 20, MaxTotalBytes: 7 << 20,
	}}
	want := AttachmentLimits{MaxFiles: 3, MaxFileBytes: 4 << 20, MaxTotalBytes: 7 << 20}
	if got := client.ProjectAttachmentLimits(); got != want {
		t.Fatalf("ProjectAttachmentLimits() = %#v, want %#v", got, want)
	}
	partial := &Core{attachmentLimits: projectconfig.AttachmentLimits{MaxFiles: 3}}
	wantPartial := AttachmentLimits{
		MaxFiles: 3, MaxFileBytes: DefaultAttachmentMaxFileBytes, MaxTotalBytes: DefaultAttachmentMaxTotalBytes,
	}
	if got := partial.ProjectAttachmentLimits(); got != wantPartial {
		t.Fatalf("partial ProjectAttachmentLimits() = %#v, want %#v", got, wantPartial)
	}
}

func TestValidateTaskInputAttachmentsAppliesDefaultInlineImageByteLimit(t *testing.T) {
	data := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, int(DefaultAttachmentMaxFileBytes+1)))
	input := taskInputWithImages(model.InputImage{ImageURL: "data:image/png;base64," + data})
	err := validateTaskInputAttachments(input, projectconfig.AttachmentLimits{})
	if err == nil || !strings.Contains(err.Error(), "attachments.maxFileBytes") {
		t.Fatalf("default inline image validation error = %v, want per-file limit", err)
	}
}

func TestRunTaskPersistsImageDataForSessionReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request model.ModelRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode model request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		stage := acceptanceRequestStage(request)
		if stage == "criteria" || stage == "review" {
			response := scriptedCriteriaResponse(request)
			if stage == "review" {
				response = scriptedAcceptanceResponse(request)
			}
			if err := writeCompletedSSE(w, response); err != nil {
				t.Errorf("write acceptance response: %v", err)
			}
			return
		}
		if request.Input == nil {
			t.Errorf("request input = %#v", request.Input)
			return
		}
		foundImage := false
		for _, item := range request.Input.Items {
			message, ok := item.(model.InputMessage)
			if !ok || message.Role != model.RoleUser {
				continue
			}
			for _, part := range message.Content.Parts {
				imagePart, ok := part.(model.InputImage)
				if ok && imagePart.ImageURL == "data:image/png;base64,YQ==" {
					foundImage = true
				}
			}
		}
		if !foundImage {
			t.Errorf("execution request did not retain the user image: %#v", request.Input)
		}
		if err := writeCompletedSSE(w, &model.ModelResponse{
			ID: "response-image", Object: "response", Status: "completed", Model: request.Model,
			Output: []model.ResponseOutputItem{model.OutputMessage{
				Role:    model.RoleAssistant,
				Content: []model.OutputContentPart{model.OutputText{Text: "I inspected the screenshot."}},
			}},
		}); err != nil {
			t.Errorf("write model response: %v", err)
		}
	}))
	defer server.Close()

	workspace, stateDir := t.TempDir(), t.TempDir()
	writeProjectSettingsConfig(t, workspace, projectconfig.Config{
		Providers: map[string]model.ProviderConfig{
			"main": {Protocol: model.ProtocolResponses, BaseURL: server.URL, APIKey: "test-key"},
		},
		Models:       map[string]model.ModelProfile{"main": {Provider: "main", Model: "vision-model"}},
		DefaultModel: "main",
	})
	client := openSettingsCore(t, workspace, stateDir)
	saved, err := client.CreateSession(context.Background(), json.RawMessage("null"))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	input, err := NewTaskInput(model.NewResponseInputItems(model.InputMessage{
		Role: model.RoleUser,
		Content: model.NewMessageContentParts(
			model.InputText{Text: "Describe this screenshot: [Image #1 · PNG · 1×1 · 1 B]"},
			model.InputImage{ImageURL: "data:image/png;base64,YQ=="},
		),
	}))
	if err != nil {
		t.Fatalf("NewTaskInput: %v", err)
	}
	if _, err := client.RunTask(context.Background(), TaskRequest{SessionID: saved.ID, Input: input}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	loaded, err := client.GetSession(context.Background(), saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(loaded.History), "data:image/png;base64,YQ==") || !strings.Contains(string(loaded.History), "Image #1") {
		t.Fatalf("session history did not retain the image and its editable marker: %s", loaded.History)
	}
	if limits := client.ProjectAttachmentLimits(); limits != (AttachmentLimits{
		MaxFiles: DefaultAttachmentMaxFiles, MaxFileBytes: DefaultAttachmentMaxFileBytes,
		MaxTotalBytes: DefaultAttachmentMaxTotalBytes,
	}) {
		t.Fatalf("effective attachment defaults = %#v", limits)
	}
}

func taskInputWithImages(images ...model.InputImage) *model.ResponseInput {
	parts := make([]model.ContentPart, 0, len(images))
	for _, image := range images {
		parts = append(parts, image)
	}
	return model.NewResponseInputItems(model.InputMessage{
		Role: model.RoleUser, Content: model.NewMessageContentParts(parts...),
	})
}
