package optimizer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestApplyCompactsOnlyCommandToolResults(t *testing.T) {
	request := map[string]any{
		"model": "test-model",
		"messages": []any{
			map[string]any{"role": "user", "content": "Keep my prompt exactly."},
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "bash", "arguments": "{}"}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "Progress 10%\rProgress 50%\rDone\nline\nline\n"},
			map[string]any{"role": "tool", "tool_call_id": "call_read", "content": "line\nline\n"},
		},
	}
	raw, _ := json.Marshal(request)
	optimized, stats := Apply(raw, "safe", "off")
	var got map[string]any
	if err := json.Unmarshal(optimized, &got); err != nil { t.Fatal(err) }
	messages := got["messages"].([]any)
	if messages[0].(map[string]any)["content"] != "Keep my prompt exactly." { t.Fatal("user prompt was modified") }
	firstTool := messages[2].(map[string]any)["content"].(string)
	if strings.Contains(firstTool, "Progress 10%") || !strings.Contains(firstTool, "Done") || !strings.Contains(firstTool, "repeated 1 more times") {
		t.Fatalf("unexpected compacted shell output: %q", firstTool)
	}
	secondTool := messages[3].(map[string]any)["content"].(string)
	if secondTool != "line\nline\n" { t.Fatalf("non-command tool result should be unchanged: %q", secondTool) }
	if stats.ToolResultsSeen != 1 || stats.ToolResultsChanged != 1 || stats.SavedBytes() == 0 {
		t.Fatalf("unexpected stats: %#v", stats)
	}
}

func TestApplyAddsOptionalCodeMode(t *testing.T) {
	raw := []byte(`{"model":"x","messages":[{"role":"user","content":"Do a small fix"}]}`)
	optimized, _ := Apply(raw, "off", "full")
	var got map[string]any
	if err := json.Unmarshal(optimized, &got); err != nil { t.Fatal(err) }
	messages := got["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" { t.Fatal("code mode should prepend a system instruction") }
	if !strings.Contains(messages[0].(map[string]any)["content"].(string), "smallest complete solution") {
		t.Fatal("expected the compact code-mode directive")
	}
}

func TestApplyOffLeavesBodyUntouched(t *testing.T) {
	raw := []byte(`{"model":"x","messages":[]}`)
	got, stats := Apply(raw, "off", "off")
	if string(got) != string(raw) || stats != (Stats{}) { t.Fatal("disabled optimization must be a no-op") }
}
