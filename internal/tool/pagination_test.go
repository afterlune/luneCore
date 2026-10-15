package tool

import "testing"

func TestParsePageRequestDefaultsAndRejectsInvalidBounds(t *testing.T) {
	request, err := parsePageRequest(nil, nil)
	if err != nil || request.offset != 0 || request.limit != defaultPageSize {
		t.Fatalf("default page request = %+v, error = %v", request, err)
	}
	for _, tc := range []struct {
		name   string
		offset *int
		limit  *int
	}{
		{name: "negative offset", offset: intPointer(-1)},
		{name: "offset beyond scan cap", offset: intPointer(maxWalkEntries + 1)},
		{name: "zero limit", limit: intPointer(0)},
		{name: "limit beyond cap", limit: intPointer(maxPageSize + 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parsePageRequest(tc.offset, tc.limit); err == nil {
				t.Fatal("parsePageRequest succeeded for invalid bounds")
			}
		})
	}
}

func intPointer(value int) *int { return &value }
