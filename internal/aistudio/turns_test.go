package aistudio

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestSanitizeThoughtSignatures(t *testing.T) {
	contents := []Content{
		{
			Role: RoleUser,
			Parts: []Part{
				{Text: "Hello, search for something"},
			},
		},
		{
			Role: RoleAssistant,
			Parts: []Part{
				{Text: "Thinking...", Thought: true, ThoughtSignature: "sig-draft"},
				{Text: "Here is the result", ThoughtSignature: "sig-text"},
				{ThoughtSignature: "sig-standalone"},
			},
		},
		{
			Role: RoleAssistant,
			Parts: []Part{
				{
					FunctionCall: &FunctionCall{
						ID:               "call_1",
						Name:             "search",
						Arguments:        json.RawMessage(`{}`),
						ThoughtSignature: "sig-call",
					},
					ThoughtSignature: "sig-call",
				},
			},
		},
	}

	cleaned := sanitizeThoughtSignatures(contents)
	if len(cleaned) != 3 {
		t.Fatalf("expected 3 contents, got %d", len(cleaned))
	}

	// Turn 0: User message unchanged
	if len(cleaned[0].Parts) != 1 || cleaned[0].Parts[0].Text != "Hello, search for something" {
		t.Errorf("unexpected turn 0: %+v", cleaned[0])
	}

	// Turn 1: Model message should have dropped standalone signature part and stripped signatures from text
	if len(cleaned[1].Parts) != 2 {
		t.Fatalf("expected 2 parts in turn 1, got %d", len(cleaned[1].Parts))
	}
	if cleaned[1].Parts[0].Text != "Thinking..." || cleaned[1].Parts[0].ThoughtSignature != "" {
		t.Errorf("part 0 thought signature not cleared: %+v", cleaned[1].Parts[0])
	}
	if cleaned[1].Parts[1].Text != "Here is the result" || cleaned[1].Parts[1].ThoughtSignature != "" {
		t.Errorf("part 1 thought signature not cleared: %+v", cleaned[1].Parts[1])
	}

	// Turn 2: Function call signatures should be cleared (encoders inject skip_thought_signature_validator per protocol)
	if len(cleaned[2].Parts) != 1 {
		t.Fatalf("expected 1 part in turn 2, got %d", len(cleaned[2].Parts))
	}
	call := cleaned[2].Parts[0].FunctionCall
	if call == nil || call.ThoughtSignature != "" {
		t.Errorf("expected empty thought signature on call, got %+v", call)
	}
	if cleaned[2].Parts[0].ThoughtSignature != "" {
		t.Errorf("expected empty thought signature on part, got %q", cleaned[2].Parts[0].ThoughtSignature)
	}
	if contents[2].Parts[0].FunctionCall.ThoughtSignature != "sig-call" {
		t.Errorf("expected original FunctionCall not to be mutated, got %q", contents[2].Parts[0].FunctionCall.ThoughtSignature)
	}
	// Calling again on already-clean contents (including FunctionCall) should hit zero-allocation fast path
	again := sanitizeThoughtSignatures(cleaned)
	if &again[0] != &cleaned[0] {
		t.Errorf("expected fast-path to return original slice without reallocation")
	}
}

func TestSanitizeThoughtSignaturesPreservesExternalMediaAndFile(t *testing.T) {
	contents := []Content{
		{
			Role: RoleUser,
			Parts: []Part{
				{
					ExternalMedia:    &ExternalMedia{MIME: "video/*", URL: "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
					ThoughtSignature: "sig-media",
				},
				{
					File:             &FileRef{ID: "file-1", Name: "doc.pdf", MIME: "application/pdf"},
					ThoughtSignature: "sig-file",
				},
				{
					ToolCall:         &ServerToolStep{ID: "srv_1", Type: "GOOGLE_SEARCH_WEB", Name: "web_search"},
					ThoughtSignature: "sig-srv-call",
				},
				{
					ToolResponse:     &ServerToolStep{ID: "srv_1", Type: "GOOGLE_SEARCH_WEB", Name: "web_search"},
					ThoughtSignature: "sig-srv-resp",
				},
			},
		},
	}

	cleaned := sanitizeThoughtSignatures(contents)
	if len(cleaned) != 1 || len(cleaned[0].Parts) != 4 {
		t.Fatalf("expected 1 content with 4 parts, got %+v", cleaned)
	}
	if cleaned[0].Parts[0].ExternalMedia == nil || cleaned[0].Parts[0].ThoughtSignature != "" {
		t.Errorf("external media part corrupted: %+v", cleaned[0].Parts[0])
	}
	if cleaned[0].Parts[1].File == nil || cleaned[0].Parts[1].ThoughtSignature != "" {
		t.Errorf("file part corrupted: %+v", cleaned[0].Parts[1])
	}
	if cleaned[0].Parts[2].ToolCall == nil || cleaned[0].Parts[2].ThoughtSignature != "" {
		t.Errorf("server tool call part corrupted: %+v", cleaned[0].Parts[2])
	}
	if cleaned[0].Parts[3].ToolResponse == nil || cleaned[0].Parts[3].ThoughtSignature != "" {
		t.Errorf("server tool response part corrupted: %+v", cleaned[0].Parts[3])
	}

	// Fast-path check: calling sanitizeThoughtSignatures again on already-clean contents should not reallocate
	again := sanitizeThoughtSignatures(cleaned)
	if &again[0] != &cleaned[0] {
		t.Errorf("expected fast-path to return original slice without reallocation")
	}
}

func TestSanitizeThoughtSignaturesParallelCallsInteractionSingleThoughtStep(t *testing.T) {
	raw, err := json.Marshal([]any{[]any{listModelRowWithField79("gemini-4-hydrogen", []any{nil, nil, 1, 2})}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := parseModelCatalog(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	model := catalog.entries["gemini-4-hydrogen"].model

	contents := []Content{
		{
			Role:  RoleUser,
			Parts: []Part{{Text: "Check weather in Tokyo and Osaka."}},
		},
		{
			Role: RoleAssistant,
			Parts: []Part{
				{ThoughtSignature: "sig-standalone"},
				{
					ThoughtSignature: "sig-call-1",
					FunctionCall: &FunctionCall{
						ID:               "call_1",
						Name:             "get_weather",
						Arguments:        json.RawMessage(`{"city":"Tokyo"}`),
						ThoughtSignature: "sig-call-1",
					},
				},
				{
					ThoughtSignature: "sig-call-2",
					FunctionCall: &FunctionCall{
						ID:               "call_2",
						Name:             "get_weather",
						Arguments:        json.RawMessage(`{"city":"Osaka"}`),
						ThoughtSignature: "sig-call-2",
					},
				},
			},
		},
		{
			Role: RoleTool,
			Parts: []Part{
				{FunctionResult: &FunctionResult{ID: "call_1", Name: "get_weather", Content: json.RawMessage(`{"temp":22}`)}},
				{FunctionResult: &FunctionResult{ID: "call_2", Name: "get_weather", Content: json.RawMessage(`{"temp":25}`)}},
			},
		},
	}

	cleaned := sanitizeThoughtSignatures(contents)
	if len(cleaned) != 3 || len(cleaned[1].Parts) != 2 {
		t.Fatalf("unexpected cleaned turns: %+v", cleaned)
	}
	for i, part := range cleaned[1].Parts {
		if part.ThoughtSignature != "" || part.FunctionCall == nil || part.FunctionCall.ThoughtSignature != "" {
			t.Fatalf("parallel call %d signature not cleared: %+v", i, part)
		}
	}
	if again := sanitizeThoughtSignatures(cleaned); &again[0] != &cleaned[0] {
		t.Fatalf("expected second sanitizeThoughtSignatures call to reuse slice without allocation")
	}

	body, _, _, err := EncodeCreateInteractionRequest(GenerateRequest{
		Model:    "gemini-4-hydrogen",
		Contents: cleaned,
	}, model)
	if err != nil {
		t.Fatal(err)
	}
	var wire []any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	steps := wire[3].([]any)[26].([]any)[0].([]any)
	// Expected steps: [user, thought(skip_thought_signature_validator), call_1, call_2, result_1, result_2]
	if len(steps) != 6 {
		t.Fatalf("expected 6 steps (strictly 1 thought signature step before parallel calls), got %d: %s", len(steps), body)
	}
	thoughtStepCount := 0
	for _, rawStep := range steps {
		step, ok := rawStep.([]any)
		if ok && len(step) >= 3 && step[2] != nil {
			thoughtStepCount++
		}
	}
	if thoughtStepCount != 1 {
		t.Fatalf("expected strictly 1 thought signature step, got %d: %s", thoughtStepCount, body)
	}
	if !reflect.DeepEqual(steps[1], []any{nil, nil, []any{"skip_thought_signature_validator"}}) {
		t.Fatalf("unexpected thought step: %v (full body: %s)", steps[1], body)
	}
	if steps[2].([]any)[3] == nil || steps[3].([]any)[3] == nil {
		t.Fatalf("expected parallel tool_call steps at indices 2 and 3: %s", body)
	}
}
