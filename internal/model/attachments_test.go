package model

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestValidateInputAttachments(t *testing.T) {
	dataURL := func(value string) string {
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(value))
	}
	taskInputWithImages := func(images ...InputImage) *ResponseInput {
		parts := make([]ContentPart, len(images))
		for i, img := range images {
			parts[i] = img
		}
		return NewResponseInputItems(InputMessage{
			Role:    RoleUser,
			Content: NewMessageContentParts(parts...),
		})
	}

	tests := []struct {
		name    string
		images  []InputImage
		limits  AttachmentLimits
		wantErr string
	}{
		{
			name:   "within per-file and total limits",
			images: []InputImage{{ImageURL: dataURL("1234")}, {ImageURL: dataURL("567")}},
			limits: AttachmentLimits{MaxFiles: 2, MaxFileBytes: 4, MaxTotalBytes: 7, Explicit: true},
		},
		{
			name:   "case-insensitive data URL header",
			images: []InputImage{{ImageURL: "DATA:IMAGE/PNG;BASE64,MTIz"}},
			limits: AttachmentLimits{MaxFiles: 5, MaxFileBytes: 3, MaxTotalBytes: 20 << 20, Explicit: true},
		},
		{
			name:   "URL-escaped base64 payload",
			images: []InputImage{{ImageURL: "data:image/png;base64,%2B%2F8%3D"}},
			limits: AttachmentLimits{MaxFiles: 5, MaxFileBytes: 2, MaxTotalBytes: 20 << 20, Explicit: true},
		},
		{
			name:    "malformed URL escape",
			images:  []InputImage{{ImageURL: "data:image/png;base64,%2G%2F8%3D"}},
			wantErr: "valid base64",
		},
		{
			name:    "too many images",
			images:  []InputImage{{ImageURL: dataURL("1")}, {ImageURL: dataURL("2")}},
			limits:  AttachmentLimits{MaxFiles: 1, MaxFileBytes: 10 << 20, MaxTotalBytes: 20 << 20, Explicit: true},
			wantErr: "attachments.maxFiles",
		},
		{
			name:    "file byte limit",
			images:  []InputImage{{ImageURL: dataURL("12345")}},
			limits:  AttachmentLimits{MaxFiles: 5, MaxFileBytes: 4, MaxTotalBytes: 20 << 20, Explicit: true},
			wantErr: "attachments.maxFileBytes",
		},
		{
			name:    "total byte limit",
			images:  []InputImage{{ImageURL: dataURL("1234")}, {ImageURL: dataURL("5678")}},
			limits:  AttachmentLimits{MaxFiles: 5, MaxFileBytes: 10 << 20, MaxTotalBytes: 7, Explicit: true},
			wantErr: "attachments.maxTotalBytes",
		},
		{
			name:    "remote image cannot satisfy byte limits",
			images:  []InputImage{{ImageURL: "https://example.test/image.png"}},
			limits:  AttachmentLimits{MaxFiles: 5, MaxFileBytes: 10 << 20, MaxTotalBytes: 1024, Explicit: true},
			wantErr: "size cannot be verified",
		},
		{
			name:   "remote image with count-only limit",
			images: []InputImage{{FileID: "file-1"}},
			limits: AttachmentLimits{MaxFiles: 1, MaxFileBytes: 10 << 20, MaxTotalBytes: 20 << 20, Explicit: false},
		},
		{
			name:   "remote image allowed when not explicit",
			images: []InputImage{{ImageURL: "https://example.test/image.png"}},
			limits: AttachmentLimits{MaxFiles: 5, MaxFileBytes: 10 << 20, MaxTotalBytes: 20 << 20, Explicit: false},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := taskInputWithImages(test.images...)
			err := ValidateInputAttachments(input, test.limits)
			if test.wantErr == "" && err != nil {
				t.Fatalf("ValidateInputAttachments error: %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("ValidateInputAttachments error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}
