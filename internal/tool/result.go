package tool

import (
	"strings"

	"github.com/afterlune/luneCore/internal/model"
)

// Result contains a tool's output and any execution error. Errors are rendered
// into the output so the model can inspect them and decide what to do next.
type Result struct {
	Output         string
	ModelOutput    *model.ToolOutput
	TranscriptOnly []model.ContentPart
	Err            error
}

func (r Result) Text() string {
	if r.Err == nil {
		return r.Output
	}
	if strings.TrimSpace(r.Output) == "" {
		return "Error: " + r.Err.Error()
	}
	return r.Output + "\nError: " + r.Err.Error()
}
