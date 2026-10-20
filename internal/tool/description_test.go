package tool

import (
	"strings"
	"testing"

	"github.com/afterlune/luneCore/internal/model"
)

func TestBuiltInDescriptionsRetainKeyConstraints(t *testing.T) {
	definitions := map[string]model.FunctionTool{
		"read_file":  ReadFileTool{}.Definition(),
		"write_file": WriteFileTool{}.Definition(),
		"edit_file":  EditFileTool{}.Definition(),
		"list_dir":   ListDirTool{}.Definition(),
		"glob":       GlobTool{}.Definition(),
		"grep":       GrepTool{}.Definition(),
		"command":    CommandTool{}.Definition(),
		"status":     CommandControlTool{}.Definition(),
		"stop":       CommandControlTool{Stop: true}.Definition(),
		"web_search": WebSearchTool{}.Definition(),
		"web_fetch":  WebFetchTool{}.Definition(),
	}
	want := map[string][]string{
		"read_file":  {"line range", "line numbers", "next start_line", "more content"},
		"write_file": {"create or replace", "complete contents"},
		"edit_file":  {"unique exact text match", "large files", "chunks"},
		"list_dir":   {"sorted", "offset and limit"},
		"glob":       {"** matches recursively", "offset and limit"},
		"grep":       {"recursively", "case-sensitive regex", ".git", ".map", "offset and limit"},
		"command":    {"workspace command", "background=true", "session", "command_status", "command_stop"},
		"status":     {"this agent's command", "byte cursor", "wait up to 5 seconds", "start receipt"},
		"stop":       {"command owned by this agent", "does not mean it succeeded"},
		"web_search": {"titles", "URLs", "snippets"},
		"web_fetch":  {"HTTP(S)", "title", "readable text"},
	}
	for name, terms := range want {
		description := strings.ToLower(definitions[name].Description)
		for _, term := range terms {
			if !strings.Contains(description, strings.ToLower(term)) {
				t.Errorf("%s description %q is missing %q", name, description, term)
			}
		}
	}
}
