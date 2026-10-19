package tool

import "testing"

func TestGlobMatcher(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "src/main.go", true},
		{"**/*.go", "src/main.txt", false},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/nested/main.go", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/x/y/c", false},
		{"**/**/a", "a", true},
		{"**/**/a", "x/y/a", true},
		{"a/**", "a", true},
		{"a/**", "a/x/y", true},
	}
	for _, test := range tests {
		t.Run(test.pattern+"/"+test.path, func(t *testing.T) {
			pattern, err := parseGlobPattern(test.pattern)
			if err != nil {
				t.Fatal(err)
			}
			matcher := newGlobMatcher(pattern)
			if got := matcher.matches(test.path); got != test.want {
				t.Fatalf("matches(%q) = %t, want %t", test.path, got, test.want)
			}
			if got := matcher.matches(test.path); got != test.want {
				t.Fatalf("second matches(%q) = %t, want %t", test.path, got, test.want)
			}
		})
	}
}
