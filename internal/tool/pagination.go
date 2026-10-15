package tool

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	defaultPageSize = 100
	maxPageSize     = 500
	pageFooterRoom  = 128
)

type pageRequest struct {
	offset int
	limit  int
}

func parsePageRequest(offset, limit *int) (pageRequest, error) {
	request := pageRequest{limit: defaultPageSize}
	if offset != nil {
		if *offset < 0 {
			return pageRequest{}, errors.New("offset must be zero or greater")
		}
		if *offset > maxWalkEntries {
			return pageRequest{}, errors.New("offset exceeds the workspace scan limit")
		}
		request.offset = *offset
	}
	if limit != nil {
		if *limit < 1 || *limit > maxPageSize {
			return pageRequest{}, errors.New("limit must be between 1 and 500")
		}
		request.limit = *limit
	}
	return request, nil
}

type pageText struct {
	builder strings.Builder
	runes   int
}

func (p *pageText) Add(record string) bool {
	count := utf8.RuneCountInString(record)
	if p.runes+count > maxToolRunes-pageFooterRoom {
		return false
	}
	p.builder.WriteString(record)
	p.runes += count
	return true
}

// AddClipped adds a bounded preview of a single oversized record. It returns
// false when there is not enough room for even a useful preview.
func (p *pageText) AddClipped(record, marker string) bool {
	remaining := maxToolRunes - pageFooterRoom - p.runes
	markerRunes := utf8.RuneCountInString(marker)
	if remaining <= markerRunes+1 {
		return false
	}
	prefix := clipRunes(record, remaining-markerRunes)
	p.builder.WriteString(prefix)
	p.builder.WriteString(marker)
	p.runes += utf8.RuneCountInString(prefix) + markerRunes
	return true
}

func (p *pageText) Empty() bool { return p.builder.Len() == 0 }

func (p *pageText) String(footer string) string {
	if p.builder.Len() == 0 {
		return footer
	}
	return p.builder.String() + "\n" + footer
}

func clipRunes(value string, maximum int) string {
	if maximum <= 0 {
		return ""
	}
	count := 0
	for index := range value {
		if count == maximum {
			return value[:index]
		}
		count++
	}
	return value
}

func pageFooter(nextOffset int, more, scanLimited bool) string {
	switch {
	case scanLimited:
		return "[scan limit reached; results may be incomplete; next_offset=" + strconv.Itoa(nextOffset) + "]"
	case more:
		return "[more results; next_offset=" + strconv.Itoa(nextOffset) + "]"
	default:
		return "[end of results]"
	}
}
