package tool

import (
	"strings"

	publictool "github.com/afterlune/luneCore/tool"
	"github.com/pmezard/go-difflib/difflib"
)

const maxDiffSourceBytes = 4 << 20

// Keep the model result separate from a bounded, structured change preview.
func fileChange(path, before, after string) *publictool.ExecutionDetails {
	if len(before) > maxDiffSourceBytes || len(after) > maxDiffSourceBytes {
		return nil
	}
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		lines := strings.SplitAfter(s, "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	old, next := split(before), split(after)
	change := publictool.FileChange{Path: path}
	bytes := 0
	add := func(kind string, oi, ni int, text string) {
		text = strings.TrimSuffix(strings.TrimSuffix(text, "\n"), "\r")
		if len(change.Lines) >= 512 || bytes+len(text) > 128*1024 {
			change.Truncated = true
			return
		}
		bytes += len(text)
		change.Lines = append(change.Lines, publictool.DiffLine{Kind: kind, OldLine: oi, NewLine: ni, Text: text})
	}
	for _, group := range difflib.NewMatcher(old, next).GetGroupedOpCodes(2) {
		for _, op := range group {
			switch op.Tag {
			case 'e':
				for i := 0; i < op.I2-op.I1; i++ {
					add("context", op.I1+i+1, op.J1+i+1, old[op.I1+i])
				}
			case 'd':
				change.Removed += op.I2 - op.I1
				for i := op.I1; i < op.I2; i++ {
					add("removed", i+1, 0, old[i])
				}
			case 'i':
				change.Added += op.J2 - op.J1
				for i := op.J1; i < op.J2; i++ {
					add("added", 0, i+1, next[i])
				}
			case 'r':
				change.Removed += op.I2 - op.I1
				change.Added += op.J2 - op.J1
				for i := op.I1; i < op.I2; i++ {
					add("removed", i+1, 0, old[i])
				}
				for i := op.J1; i < op.J2; i++ {
					add("added", 0, i+1, next[i])
				}
			}
		}
	}
	return &publictool.ExecutionDetails{Changes: []publictool.FileChange{change}}
}
