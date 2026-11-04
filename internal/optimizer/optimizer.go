package optimizer

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

type Stats struct {
	ToolResultsSeen    int
	ToolResultsChanged int
	OriginalBytes      int
	OptimizedBytes     int
}

func (s Stats) SavedBytes() int {
	saved := s.OriginalBytes - s.OptimizedBytes
	if saved < 0 { return 0 }
	return saved
}

var ansiPattern = regexp.MustCompile("\x1b\\[[0-?]*[ -/]*[@-~]|\x1b\\][^\x07]*(?:\x07|\x1b\\\\)|\x1b[@-_]")

// Apply performs deterministic cleanup only on recognized shell-tool results.
// User prompts, assistant messages and file-read results are untouched.
func Apply(body []byte, saverMode, codeMode string) ([]byte, Stats) {
	var stats Stats
	mode := strings.ToLower(strings.TrimSpace(saverMode))
	code := strings.ToLower(strings.TrimSpace(codeMode))
	if mode != "safe" && mode != "balanced" { mode = "off" }
	if code != "lite" && code != "full" && code != "ultra" { code = "off" }
	if mode == "off" && code == "off" { return body, stats }

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil { return body, stats }
	var messages []json.RawMessage
	if raw := payload["messages"]; len(raw) > 0 { _ = json.Unmarshal(raw, &messages) }

	toolNames := make(map[string]string)
	for _, raw := range messages {
		var message struct {
			Role string `json:"role"`
			ToolCalls []struct {
				ID string `json:"id"`
				Function struct { Name string `json:"name"` } `json:"function"`
			} `json:"tool_calls"`
		}
		if json.Unmarshal(raw, &message) != nil || message.Role != "assistant" { continue }
		for _, call := range message.ToolCalls {
			if call.ID != "" { toolNames[call.ID] = call.Function.Name }
		}
	}

	changed := false
	if mode != "off" && len(messages) > 0 {
		for i, raw := range messages {
			var message map[string]json.RawMessage
			if json.Unmarshal(raw, &message) != nil { continue }
			var role string
			_ = json.Unmarshal(message["role"], &role)
			if role != "tool" && role != "function" { continue }
			var toolCallID, toolName string
			_ = json.Unmarshal(message["tool_call_id"], &toolCallID)
			_ = json.Unmarshal(message["name"], &toolName)
			if toolName == "" { toolName = toolNames[toolCallID] }
			if !isCommandTool(toolName) { continue }
			var content string
			if len(message["content"]) == 0 || json.Unmarshal(message["content"], &content) != nil { continue }
			stats.ToolResultsSeen++
			stats.OriginalBytes += len(content)
			compacted := compactToolOutput(content)
			stats.OptimizedBytes += len(compacted)
			if compacted == content { continue }
			message["content"], _ = json.Marshal(compacted)
			messages[i], _ = json.Marshal(message)
			stats.ToolResultsChanged++
			changed = true
		}
		payload["messages"], _ = json.Marshal(messages)
	}
	if code != "off" && addCodeMode(payload, code) { changed = true }
	if !changed { return body, stats }
	encoded, err := json.Marshal(payload)
	if err != nil { return body, Stats{} }
	return encoded, stats
}

func isCommandTool(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, part := range []string{"bash", "shell", "terminal", "command", "exec"} {
		if name == part || strings.HasSuffix(name, "."+part) || strings.Contains(name, "run_"+part) || strings.Contains(name, part+"_command") {
			return true
		}
	}
	return false
}

func compactToolOutput(value string) string {
	clean := ansiPattern.ReplaceAllString(value, "")
	if json.Valid([]byte(clean)) {
		var compact bytes.Buffer
		if json.Compact(&compact, []byte(clean)) == nil && compact.Len() < len(clean) {
			return compact.String()
		}
	}
	clean = terminalLines(clean)
	lines := strings.Split(clean, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] && lines[i] != "" { j++ }
		out = append(out, lines[i])
		if j-i > 1 { out = append(out, "[previous line repeated "+strconv.Itoa(j-i-1)+" more times]") }
		i = j
	}
	return strings.Join(out, "\n")
}

// A carriage return redraws the current terminal line. Keep the final version,
// not every transient percentage or spinner update.
func terminalLines(value string) string {
	var lines []string
	var line strings.Builder
	for _, r := range value {
		switch r {
		case '\r':
			line.Reset()
		case '\n':
			lines = append(lines, line.String())
			line.Reset()
		default:
			line.WriteRune(r)
		}
	}
	if line.Len() > 0 { lines = append(lines, line.String()) }
	return strings.Join(lines, "\n")
}

func addCodeMode(payload map[string]json.RawMessage, mode string) bool {
	var messages []json.RawMessage
	if raw := payload["messages"]; len(raw) > 0 {
		if json.Unmarshal(raw, &messages) != nil { return false }
	}
	directive := codeDirective(mode)
	if directive == "" { return false }
	if len(messages) > 0 {
		var first map[string]json.RawMessage
		if json.Unmarshal(messages[0], &first) == nil {
			var role, content string
			_ = json.Unmarshal(first["role"], &role)
			if role == "system" && json.Unmarshal(first["content"], &content) == nil {
				first["content"], _ = json.Marshal(content + "\n\n" + directive)
				messages[0], _ = json.Marshal(first)
				payload["messages"], _ = json.Marshal(messages)
				return true
			}
		}
	}
	systemMessage, _ := json.Marshal(map[string]string{"role": "system", "content": directive})
	messages = append([]json.RawMessage{systemMessage}, messages...)
	payload["messages"], _ = json.Marshal(messages)
	return true
}

func codeDirective(mode string) string {
	switch mode {
	case "lite":
		return "For coding tasks, prefer the smallest complete change. Reuse existing project patterns and avoid unnecessary dependencies or abstractions. Do not omit correctness, security, error handling, accessibility, or requested tests."
	case "full":
		return "For coding tasks, inspect the touched code and its callers first. Prefer the smallest complete solution: reuse existing helpers and platform features before adding dependencies or abstractions. Keep the requested scope, preserve validation, security, accessibility and error handling, and add focused tests for non-trivial changes. Do not shorten code at the expense of correctness."
	case "ultra":
		return "For coding tasks, challenge unnecessary complexity before implementing it. Inspect existing code, callers and tests; choose the smallest complete solution and reuse existing patterns or standard-library features. Avoid speculative features, wrappers and dependencies. Preserve security, validation, accessibility, error handling and tests. State material trade-offs rather than silently omitting required work."
	default:
		return ""
	}
}
