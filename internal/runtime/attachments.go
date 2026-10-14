package runtime

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	projectconfig "github.com/afterlune/luneCore/internal/config"
	"github.com/afterlune/luneCore/internal/model"
)

func validateTaskInputAttachments(input *model.ResponseInput, limits projectconfig.AttachmentLimits) error {
	images := taskInputImages(input)
	maxFiles := limits.MaxFiles
	if maxFiles <= 0 {
		maxFiles = projectconfig.DefaultAttachmentMaxFiles
	}
	if len(images) > maxFiles {
		return fmt.Errorf("input has %d image attachment(s); attachments.maxFiles allows %d", len(images), maxFiles)
	}
	totalBytes := int64(0)
	for index, image := range images {
		attachmentNumber := index + 1
		if image.ImageURL != "" && image.FileID != "" {
			return fmt.Errorf("attachment %d must use either image_url or file_id, not both", attachmentNumber)
		}
		if image.ImageURL == "" && image.FileID == "" {
			return fmt.Errorf("attachment %d has no image_url or file_id", attachmentNumber)
		}

		var size int64
		if hasFoldedPrefix(image.ImageURL, "data:") {
			maxFileBytes := limits.MaxFileBytes
			if maxFileBytes <= 0 {
				maxFileBytes = projectconfig.DefaultAttachmentMaxFileBytes
			}
			maxTotalBytes := limits.MaxTotalBytes
			if maxTotalBytes <= 0 {
				maxTotalBytes = projectconfig.DefaultAttachmentMaxTotalBytes
			}
			remaining := maxTotalBytes - totalBytes
			if remaining <= 0 {
				return fmt.Errorf("image attachments exceed attachments.maxTotalBytes (%d bytes allowed)", maxTotalBytes)
			}
			byteLimit := maxFileBytes
			if remaining < byteLimit {
				byteLimit = remaining
			}
			parsedSize, err := dataURLImageSize(image.ImageURL, byteLimit, true)
			if err != nil {
				return fmt.Errorf("attachment %d: %w", attachmentNumber, err)
			}
			size = parsedSize
		} else if limits.MaxFileBytes > 0 || limits.MaxTotalBytes > 0 {
			return fmt.Errorf("attachment %d uses a remote URL or file_id whose size cannot be verified; supply a data:image/...;base64 payload or remove the byte limit", attachmentNumber)
		}

		maxFileBytes := limits.MaxFileBytes
		if maxFileBytes <= 0 {
			maxFileBytes = projectconfig.DefaultAttachmentMaxFileBytes
		}
		maxTotalBytes := limits.MaxTotalBytes
		if maxTotalBytes <= 0 {
			maxTotalBytes = projectconfig.DefaultAttachmentMaxTotalBytes
		}
		if hasFoldedPrefix(image.ImageURL, "data:") && size > maxFileBytes {
			return fmt.Errorf("attachment %d is %d bytes; attachments.maxFileBytes allows %d", attachmentNumber, size, maxFileBytes)
		}
		if hasFoldedPrefix(image.ImageURL, "data:") && (size > maxTotalBytes || totalBytes > maxTotalBytes-size) {
			return fmt.Errorf("image attachments exceed attachments.maxTotalBytes (%d bytes allowed)", maxTotalBytes)
		}
		totalBytes += size
	}
	return nil
}

func taskInputImages(input *model.ResponseInput) []model.InputImage {
	if input == nil {
		return nil
	}
	var images []model.InputImage
	appendParts := func(parts []model.ContentPart) {
		for _, part := range parts {
			if image, ok := part.(model.InputImage); ok {
				images = append(images, image)
			}
		}
	}
	for _, item := range input.Items {
		switch item := item.(type) {
		case model.InputMessage:
			appendParts(item.Content.Parts)
		case model.InputFunctionCallOutput:
			appendParts(item.Output.Parts)
		case model.InputCustomToolCallOutput:
			appendParts(item.Output.Parts)
		}
	}
	return images
}

func dataURLImageSize(value string, byteLimit int64, hasByteLimit bool) (int64, error) {
	header, payload, ok := strings.Cut(value, ",")
	if !ok || !hasFoldedPrefix(header, "data:image/") || !hasFoldedSuffix(header, ";base64") {
		return 0, errors.New("image data must use a base64 data:image URL")
	}
	if payload == "" {
		return 0, errors.New("image data is empty")
	}
	var reader io.Reader = base64.NewDecoder(base64.StdEncoding.Strict(), &urlPercentReader{value: payload})
	const maxInt64 = int64(1<<63 - 1)
	if hasByteLimit && byteLimit < maxInt64 {
		reader = io.LimitReader(reader, byteLimit+1)
	}
	size, err := io.Copy(io.Discard, reader)
	if err != nil {
		return 0, fmt.Errorf("image data is not valid base64: %w", err)
	}
	return size, nil
}

type urlPercentReader struct {
	value  string
	offset int
}

func (r *urlPercentReader) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	read := 0
	for read < len(destination) && r.offset < len(r.value) {
		value := r.value[r.offset]
		if value != '%' {
			destination[read] = value
			read++
			r.offset++
			continue
		}
		if r.offset+2 >= len(r.value) {
			return read, errors.New("incomplete percent escape")
		}
		high, highOK := hexValue(r.value[r.offset+1])
		low, lowOK := hexValue(r.value[r.offset+2])
		if !highOK || !lowOK {
			return read, errors.New("invalid percent escape")
		}
		destination[read] = high<<4 | low
		read++
		r.offset += 3
	}
	if read == 0 {
		return 0, io.EOF
	}
	return read, nil
}

func hexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func hasFoldedPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}

func hasFoldedSuffix(value, suffix string) bool {
	return len(value) >= len(suffix) && strings.EqualFold(value[len(value)-len(suffix):], suffix)
}
