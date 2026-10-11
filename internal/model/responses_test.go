package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponseInputRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		json  string
		check func(t *testing.T, input *ResponseInput)
	}{
		{
			name: "plain text",
			json: `"hello"`,
			check: func(t *testing.T, input *ResponseInput) {
				if input.Text == nil || *input.Text != "hello" || input.Items != nil {
					t.Fatalf("unexpected input: %#v", input)
				}
			},
		},
		{
			name: "tagged items and unknown item",
			json: `[
				{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"https://example.test/image.png","detail":"low"}]},
				{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
				{"type":"function_call_output","call_id":"call_1","output":"done"},
				{"type":"custom_tool_call","call_id":"call_2","name":"apply_patch","input":"*** Begin Patch"},
				{"type":"custom_tool_call_output","call_id":"call_2","output":[{"type":"input_text","text":"patched"}]},
				{"type":"reasoning","content":[{"type":"reasoning_text","text":"thinking"}]},
				{"type":"future_item","value":3}
			]`,
			check: func(t *testing.T, input *ResponseInput) {
				if input.Text != nil || len(input.Items) != 7 {
					t.Fatalf("unexpected input: %#v", input)
				}
				if _, ok := input.Items[0].(InputMessage); !ok {
					t.Fatalf("role-only message decoded as %T", input.Items[0])
				}
				if _, ok := input.Items[1].(InputFunctionCall); !ok {
					t.Fatalf("function call decoded as %T", input.Items[1])
				}
				if _, ok := input.Items[2].(InputFunctionCallOutput); !ok {
					t.Fatalf("function output decoded as %T", input.Items[2])
				}
				if _, ok := input.Items[3].(InputCustomToolCall); !ok {
					t.Fatalf("custom call decoded as %T", input.Items[3])
				}
				if _, ok := input.Items[4].(InputCustomToolCallOutput); !ok {
					t.Fatalf("custom output decoded as %T", input.Items[4])
				}
				if _, ok := input.Items[5].(InputReasoning); !ok {
					t.Fatalf("reasoning decoded as %T", input.Items[5])
				}
				if _, ok := input.Items[6].(UnknownInputItem); !ok {
					t.Fatalf("unknown item decoded as %T", input.Items[6])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input ResponseInput
			if err := json.Unmarshal([]byte(tt.json), &input); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			tt.check(t, &input)

			encoded, err := json.Marshal(&input)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var roundTripped ResponseInput
			if err := json.Unmarshal(encoded, &roundTripped); err != nil {
				t.Fatalf("unmarshal round trip: %v", err)
			}
			tt.check(t, &roundTripped)
			if strings.Contains(tt.json, "future_item") && !strings.Contains(string(encoded), "future_item") {
				t.Fatalf("unknown item was not preserved: %s", encoded)
			}
		})
	}
}

func TestResponseRequestToolsAndUnionFields(t *testing.T) {
	const payload = `{
		"model":"deepseek-flash",
		"instructions":"Be concise",
		"input":"hello",
		"reasoning":{"effort":"high"},
		"max_output_tokens":128,
		"stream":true,
		"temperature":0.3,
		"top_p":0.95,
		"top_logprobs":2,
		"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"},"strict":true}},
		"tools":[
			{"type":"function","name":"lookup","parameters":{"type":"object"}},
			{"type":"custom","name":"apply_patch"},
			{"type":"future_tool","value":1}
		],
		"tool_choice":{"type":"function","name":"lookup"},
		"user":"caller-1"
	}`
	var request ResponseRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if request.Input == nil || request.Input.Text == nil || *request.Input.Text != "hello" {
		t.Fatalf("unexpected input: %#v", request.Input)
	}
	if len(request.Tools) != 3 {
		t.Fatalf("got %d tools, want 3", len(request.Tools))
	}
	if _, ok := request.Tools[0].(FunctionTool); !ok {
		t.Fatalf("function tool decoded as %T", request.Tools[0])
	}
	if _, ok := request.Tools[1].(CustomTool); !ok {
		t.Fatalf("custom tool decoded as %T", request.Tools[1])
	}
	if _, ok := request.Tools[2].(UnknownResponseTool); !ok {
		t.Fatalf("unknown tool decoded as %T", request.Tools[2])
	}
	if request.ToolChoice == nil || request.ToolChoice.Named == nil || request.ToolChoice.Named.Name != "lookup" {
		t.Fatalf("unexpected tool choice: %#v", request.ToolChoice)
	}

	encoded, err := json.Marshal(&request)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTripped ResponseRequest
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("unmarshal round trip: %v", err)
	}
	if len(roundTripped.Tools) != 3 || roundTripped.Text == nil || roundTripped.Text.Format.Type != TextFormatJSONSchema {
		t.Fatalf("request fields lost on round trip: %#v", roundTripped)
	}
}

func TestMessageContentAndToolOutputStringOrParts(t *testing.T) {
	for _, raw := range []string{
		`"plain text"`,
		`[{"type":"input_text","text":"text"},{"type":"input_image","file_id":"file-api-1"}]`,
	} {
		var content MessageContent
		if err := json.Unmarshal([]byte(raw), &content); err != nil {
			t.Fatalf("message content %s: %v", raw, err)
		}
		encoded, err := json.Marshal(content)
		if err != nil {
			t.Fatalf("marshal message content: %v", err)
		}
		var again MessageContent
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatalf("round trip message content: %v", err)
		}
	}

	var output ToolOutput
	if err := json.Unmarshal([]byte(`[{"type":"output_text","text":"ok"}]`), &output); err != nil {
		t.Fatalf("unmarshal tool output: %v", err)
	}
	if len(output.Parts) != 1 {
		t.Fatalf("got %d output parts, want 1", len(output.Parts))
	}
	if _, ok := output.Parts[0].(OutputText); !ok {
		t.Fatalf("tool output part decoded as %T", output.Parts[0])
	}
}

func TestResponseOutputItemsAndUsage(t *testing.T) {
	const payload = `{
		"id":"resp_1",
		"object":"response",
		"created_at":1753000000,
		"status":"completed",
		"error":null,
		"incomplete_details":null,
		"model":"deepseek-flash",
		"output":[
			{"type":"reasoning","id":"rs_1","status":"completed","content":[{"type":"reasoning_text","text":"thinking"}],"summary":[]},
			{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]},
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
			{"type":"custom_tool_call","call_id":"call_2","name":"apply_patch","input":"patch"},
			{"type":"future_output","payload":true}
		],
		"usage":{"input_tokens":22,"input_tokens_details":{"cached_tokens":3},"output_tokens":29,"output_tokens_details":{"reasoning_tokens":27},"total_tokens":51},
		"store":false,
		"parallel_tool_calls":true,
		"previous_response_id":null
	}`
	var response Response
	if err := json.Unmarshal([]byte(payload), &response); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(response.Output) != 5 {
		t.Fatalf("got %d output items, want 5", len(response.Output))
	}
	if _, ok := response.Output[0].(OutputReasoning); !ok {
		t.Fatalf("reasoning decoded as %T", response.Output[0])
	}
	message, ok := response.Output[1].(OutputMessage)
	if !ok {
		t.Fatalf("message decoded as %T", response.Output[1])
	}
	if len(message.Content) != 1 {
		t.Fatalf("got %d message parts, want 1", len(message.Content))
	}
	if _, ok := message.Content[0].(OutputText); !ok {
		t.Fatalf("message content decoded as %T", message.Content[0])
	}
	if _, ok := response.Output[2].(OutputFunctionCall); !ok {
		t.Fatalf("function call decoded as %T", response.Output[2])
	}
	if _, ok := response.Output[3].(OutputCustomToolCall); !ok {
		t.Fatalf("custom call decoded as %T", response.Output[3])
	}
	if _, ok := response.Output[4].(UnknownOutputItem); !ok {
		t.Fatalf("unknown output decoded as %T", response.Output[4])
	}
	if response.Usage.InputTokensDetails.CachedTokens != 3 || response.Usage.OutputTokensDetails.ReasoningTokens != 27 {
		t.Fatalf("usage details lost: %#v", response.Usage)
	}

	encoded, err := json.Marshal(&response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTripped Response
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("unmarshal round trip: %v", err)
	}
	if !strings.Contains(string(encoded), `"store":false`) || !strings.Contains(string(encoded), `"previous_response_id":null`) {
		t.Fatalf("DeepSeek compatibility fields missing: %s", encoded)
	}
}

func TestResponseStreamEvents(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		typeName ResponseEventType
		check    func(t *testing.T, event ResponseStreamEvent)
	}{
		{
			name:     "created",
			json:     `{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","output":[]}}`,
			typeName: EventResponseCreated,
		},
		{
			name:     "in progress",
			json:     `{"type":"response.in_progress","sequence_number":1}`,
			typeName: EventResponseInProgress,
		},
		{
			name:     "output text delta",
			json:     `{"type":"response.output_text.delta","sequence_number":11,"item_id":"msg_1","output_index":1,"content_index":0,"delta":"Hello"}`,
			typeName: EventOutputTextDelta,
			check: func(t *testing.T, event ResponseStreamEvent) {
				if event.Delta != "Hello" || event.ItemID != "msg_1" || event.OutputIndex == nil || *event.OutputIndex != 1 {
					t.Fatalf("unexpected delta event: %#v", event)
				}
			},
		},
		{
			name:     "output text done",
			json:     `{"type":"response.output_text.done","sequence_number":13,"item_id":"msg_1","output_index":1,"content_index":0,"text":"Hello"}`,
			typeName: EventOutputTextDone,
			check: func(t *testing.T, event ResponseStreamEvent) {
				if event.Text != "Hello" {
					t.Fatalf("unexpected text event: %#v", event)
				}
			},
		},
		{
			name:     "reasoning text delta",
			json:     `{"type":"response.reasoning_text.delta","sequence_number":5,"item_id":"rs_1","output_index":0,"content_index":0,"delta":"thinking"}`,
			typeName: EventReasoningTextDelta,
		},
		{
			name:     "reasoning text done",
			json:     `{"type":"response.reasoning_text.done","sequence_number":6,"item_id":"rs_1","output_index":0,"content_index":0,"text":"thinking"}`,
			typeName: EventReasoningTextDone,
		},
		{
			name:     "function arguments done",
			json:     `{"type":"response.function_call_arguments.done","sequence_number":12,"item_id":"fc_1","output_index":0,"name":"lookup","arguments":"{}"}`,
			typeName: EventFunctionCallArgumentsDone,
			check: func(t *testing.T, event ResponseStreamEvent) {
				if event.Name != "lookup" || event.Arguments != "{}" {
					t.Fatalf("unexpected function event: %#v", event)
				}
			},
		},
		{
			name:     "function arguments delta",
			json:     `{"type":"response.function_call_arguments.delta","sequence_number":10,"item_id":"fc_1","output_index":0,"delta":"{\"city\":"}`,
			typeName: EventFunctionCallArgumentsDelta,
		},
		{
			name:     "custom tool input delta",
			json:     `{"type":"response.custom_tool_call_input.delta","sequence_number":14,"item_id":"ct_1","output_index":0,"delta":"*** Begin Patch"}`,
			typeName: EventCustomToolCallInputDelta,
		},
		{
			name:     "custom tool input done",
			json:     `{"type":"response.custom_tool_call_input.done","sequence_number":15,"item_id":"ct_1","output_index":0,"name":"apply_patch","input":"*** Begin Patch"}`,
			typeName: EventCustomToolCallInputDone,
		},
		{
			name:     "item added",
			json:     `{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}}`,
			typeName: EventOutputItemAdded,
			check: func(t *testing.T, event ResponseStreamEvent) {
				if _, ok := event.Item.(OutputFunctionCall); !ok {
					t.Fatalf("event item decoded as %T", event.Item)
				}
			},
		},
		{
			name:     "item done",
			json:     `{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}`,
			typeName: EventOutputItemDone,
		},
		{
			name:     "content part added",
			json:     `{"type":"response.content_part.added","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
			typeName: EventContentPartAdded,
		},
		{
			name:     "content part",
			json:     `{"type":"response.content_part.done","sequence_number":4,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"Hello","annotations":[]}}`,
			typeName: EventContentPartDone,
			check: func(t *testing.T, event ResponseStreamEvent) {
				if _, ok := event.Part.(OutputText); !ok {
					t.Fatalf("event part decoded as %T", event.Part)
				}
			},
		},
		{
			name:     "terminal response",
			json:     `{"type":"response.completed","sequence_number":20,"response":{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2}}}`,
			typeName: EventResponseCompleted,
			check: func(t *testing.T, event ResponseStreamEvent) {
				if event.Response == nil || event.Response.ID != "resp_1" {
					t.Fatalf("terminal response missing: %#v", event.Response)
				}
			},
		},
		{
			name:     "incomplete response",
			json:     `{"type":"response.incomplete","sequence_number":21,"response":{"id":"resp_2","object":"response","status":"incomplete","output":[],"incomplete_details":{"reason":"max_output_tokens"}}}`,
			typeName: EventResponseIncomplete,
		},
		{
			name:     "failed response",
			json:     `{"type":"response.failed","sequence_number":22,"response":{"id":"resp_3","object":"response","status":"failed","output":[],"error":{"code":"server_error","message":"failed"}}}`,
			typeName: EventResponseFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var event ResponseStreamEvent
			if err := json.Unmarshal([]byte(tt.json), &event); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if event.Type != tt.typeName {
				t.Fatalf("got type %q, want %q", event.Type, tt.typeName)
			}
			if tt.check != nil {
				tt.check(t, event)
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var roundTripped ResponseStreamEvent
			if err := json.Unmarshal(encoded, &roundTripped); err != nil {
				t.Fatalf("unmarshal round trip: %v", err)
			}
			if roundTripped.Type != event.Type || roundTripped.SequenceNumber != event.SequenceNumber {
				t.Fatalf("stream event changed on round trip: %#v", roundTripped)
			}
		})
	}
}
