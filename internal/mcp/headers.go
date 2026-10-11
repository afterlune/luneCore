package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type headerBinding struct {
	path       []string
	headerName string
	valueType  string
}

func parseHeaderBindings(raw json.RawMessage) ([]headerBinding, bool) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var schema map[string]any
	if decoder.Decode(&schema) != nil || schema == nil {
		return nil, false
	}
	var bindings []headerBinding
	seen := make(map[string]bool)
	valid := true
	var walkProperties func(map[string]any, []string, bool)
	walkProperties = func(value map[string]any, path []string, propertySchema bool) {
		if !valid {
			return
		}
		annotation, annotated := value["x-mcp-header"]
		if annotated {
			headerName, ok := annotation.(string)
			if !propertySchema || !ok || !validHeaderName(headerName) {
				valid = false
				return
			}
			typeName, ok := value["type"].(string)
			if !ok || typeName != "string" && typeName != "integer" && typeName != "boolean" {
				valid = false
				return
			}
			key := strings.ToLower(headerName)
			if seen[key] {
				valid = false
				return
			}
			seen[key] = true
			bindings = append(bindings, headerBinding{path: append([]string(nil), path...), headerName: headerName, valueType: typeName})
		}
		for key, child := range value {
			if key == "properties" {
				properties, ok := child.(map[string]any)
				if !ok {
					valid = false
					return
				}
				for propertyName, property := range properties {
					propertyObject, ok := property.(map[string]any)
					if !ok {
						continue
					}
					walkProperties(propertyObject, append(append([]string(nil), path...), propertyName), true)
				}
				continue
			}
			if containsHeaderAnnotation(child) {
				valid = false
				return
			}
		}
	}
	walkProperties(schema, nil, false)
	return bindings, valid
}

func containsHeaderAnnotation(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "x-mcp-header" || containsHeaderAnnotation(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if containsHeaderAnnotation(child) {
				return true
			}
		}
	}
	return false
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range []byte(value) {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		if strings.ContainsRune("!#$%&'*+-.^_`|~", rune(char)) {
			continue
		}
		return false
	}
	return true
}

func argumentAt(arguments map[string]any, path []string) (any, bool) {
	var current any = arguments
	for _, segment := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func encodeHeaderParameter(value any, valueType string) (string, bool) {
	var text string
	switch valueType {
	case "string":
		var ok bool
		text, ok = value.(string)
		if !ok {
			return "", false
		}
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return "", false
		}
		integer, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil || math.Abs(float64(integer)) > 9007199254740991 {
			return "", false
		}
		text = strconv.FormatInt(integer, 10)
	case "boolean":
		boolean, ok := value.(bool)
		if !ok {
			return "", false
		}
		text = strconv.FormatBool(boolean)
	default:
		return "", false
	}
	return encodeHeaderValue(text), true
}

func (binding headerBinding) String() string {
	return fmt.Sprintf("%s=%s", strings.Join(binding.path, "."), binding.headerName)
}
