package tool

import (
	"encoding/json"

	"github.com/afterlune/luneCore/internal/model"
)

// functionDefinition builds a strict function tool definition from a JSON
// schema. Every built-in tool describes itself through this helper.
func functionDefinition(name, description, schema string) model.FunctionTool {
	strict := true
	return model.FunctionTool{
		Name:        name,
		Description: description,
		Parameters:  json.RawMessage(schema),
		Strict:      &strict,
	}
}
