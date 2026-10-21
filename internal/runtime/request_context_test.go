package runtime

import (
	publicmodel "github.com/afterlune/luneCore/model"
	"strings"
)

func requestHostText(request publicmodel.ModelRequest) string {
	var parts []string
	for _, item := range request.Input.Items {
		if msg, ok := item.(publicmodel.InputMessage); ok && msg.Content.Text != nil {
			parts = append(parts, *msg.Content.Text)
		}
	}
	return strings.Join(parts, "\n")
}
