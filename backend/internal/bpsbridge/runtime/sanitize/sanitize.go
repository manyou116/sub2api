// Package sanitize adapts a Codex request into the shape Basispoints accepts.
//
// Codex always advertises a fixed tool set, including web search. Basispoints
// cannot perform live web search or image generation, so those entries must be
// removed rather than forwarded. Everything else -- shell, patches, MCP tools,
// namespaces -- is a client-side tool the model merely describes, and rides
// through the run_officejs transport intact.
package sanitize

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Report describes what was removed, so the caller can log or surface it.
type Report struct {
	// RemovedServerTools lists server-executed tools that were dropped.
	RemovedServerTools []string
	// ForcedToolChoice is set when tool_choice pointed at a dropped server tool.
	ForcedToolChoice string
}

// Empty reports whether the request needed no changes.
func (r Report) Empty() bool {
	return len(r.RemovedServerTools) == 0 && r.ForcedToolChoice == ""
}

// serverExecuted reports whether a tool runs on OpenAI's servers rather than on
// the client. Only these are unavailable through the Excel gateway.
func serverExecuted(tool gjson.Result) (string, bool) {
	kind := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
	switch kind {
	case "image_generation", "web_search", "web_search_preview",
		"web_search_preview_2025_03_11", "web_search_2025_08_26",
		"code_interpreter", "file_search", "computer_use_preview",
		"mcp", "web_search_2025_08_26_preview":
		return kind, true
	}
	return "", false
}

// Apply removes server-executed tools and rewrites tool_choice when it named one.
//
// A non-empty error means the body is not valid JSON and must not be forwarded.
func Apply(raw []byte) ([]byte, Report, error) {
	var report Report
	if !gjson.ValidBytes(raw) {
		return nil, report, fmt.Errorf("request body is not valid JSON")
	}

	tools := gjson.GetBytes(raw, "tools")
	out := raw

	if tools.IsArray() {
		kept := make([]json.RawMessage, 0, len(tools.Array()))
		removed := make(map[string]bool, 4)
		tools.ForEach(func(_, tool gjson.Result) bool {
			if kind, bad := serverExecuted(tool); bad {
				removed[kind] = true
				report.RemovedServerTools = append(report.RemovedServerTools, kind)
				return true
			}
			kept = append(kept, json.RawMessage(tool.Raw))
			return true
		})

		if len(report.RemovedServerTools) > 0 {
			var err error
			if len(kept) == 0 {
				// An empty tools array is rejected outright, so drop the key.
				out, err = sjson.DeleteBytes(out, "tools")
			} else {
				out, err = sjson.SetRawBytes(out, "tools", mustMarshal(kept))
			}
			if err != nil {
				return nil, report, fmt.Errorf("rewrite tools: %w", err)
			}
		}

		// tool_choice naming anything the gateway cannot run would 400 upstream,
		// whether or not that tool appeared in this request's tools array.
		choice := gjson.GetBytes(out, "tool_choice")
		if choice.Exists() && choice.Type == gjson.JSON {
			name := strings.ToLower(strings.TrimSpace(choice.Get("name").String()))
			kind := strings.ToLower(strings.TrimSpace(choice.Get("type").String()))
			if removed[name] || removed[kind] || namesServerTool(name) || namesServerTool(kind) {
				rewritten, err := sjson.SetBytes(out, "tool_choice", "auto")
				if err != nil {
					return nil, report, fmt.Errorf("rewrite tool_choice: %w", err)
				}
				out = rewritten
				report.ForcedToolChoice = "auto"
			}
		}
	}

	return out, report, nil
}

// namesServerTool reports whether a tool_choice name or type refers to something
// the gateway executes server-side.
func namesServerTool(s string) bool {
	if s == "" {
		return false
	}
	switch s {
	case "image_generation", "web_search", "web_search_preview", "code_interpreter",
		"file_search", "computer_use_preview":
		return true
	}
	return strings.Contains(s, "web_search") || strings.Contains(s, "image_generation")
}

func mustMarshal(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("[]")
	}
	return raw
}
