// Package basispoints adapts Responses clients to the ChatGPT Excel gateway.
package basispoints

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

const ResponsesURL = "https://bps.openai.com/basispoints/api/responses"

type object = map[string]any

type Bridge struct {
	RequestedEffort  string
	Effort           string
	Warnings         []string
	tools            map[string]tool
	unsupportedTools map[string]bool
	structured       *structuredOutput
	replay           *ReplayCache
	scope            string
}

func decode(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

func text(value any) string {
	s, _ := value.(string)
	return s
}

// NormalizeEffort caps unsupported high tiers explicitly instead of falling back to medium.
func NormalizeEffort(effort string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "medium":
		return "medium", nil
	case "low", "high":
		return strings.ToLower(strings.TrimSpace(effort)), nil
	case "xhigh", "x-high", "extra-high", "extra_high", "max", "ultra":
		return "xhigh", nil
	case "none", "minimal":
		return "low", nil
	default:
		return "", fmt.Errorf("basispoints reasoning effort %q is unsupported", effort)
	}
}

func fingerprint(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func message(role, content string) object {
	return object{"type": "message", "role": role, "content": []any{object{"type": "input_text", "text": content}}}
}

// PrepareOptions separates server-owned upstream task identity from local
// history/replay scope. SessionID preserves the standalone compatibility path.
// UpstreamTaskID and UpstreamTurnID are trusted server options, not client fields.
type PrepareOptions struct {
	SessionID      string
	UpstreamTaskID string
	UpstreamTurnID string
}

func validUpstreamIdentity(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// Validate rejects partial or malformed trusted identity overrides before any
// request adaptation, image rewriting, or replay-cache writes are attempted.
func (o PrepareOptions) Validate() error {
	if o.UpstreamTaskID == "" && o.UpstreamTurnID == "" {
		return nil
	}
	if !validUpstreamIdentity(o.UpstreamTaskID) || !validUpstreamIdentity(o.UpstreamTurnID) {
		return fmt.Errorf("invalid server-owned upstream task identity")
	}
	return nil
}

// Prepare preserves the legacy content-derived conversation identity. Callers
// with an explicit logical session use PrepareWithOptions instead.
func Prepare(raw []byte, scope string, replay *ReplayCache) ([]byte, *Bridge, error) {
	return PrepareWithOptions(raw, scope, replay, PrepareOptions{})
}

// PrepareWithOptions preserves the requested model and uses a whitelist for the
// Excel wire body. A logical session changes task identity only, never history.
func PrepareWithOptions(raw []byte, scope string, replay *ReplayCache, options PrepareOptions) ([]byte, *Bridge, error) {
	hasUpstreamIdentity := options.UpstreamTaskID != "" || options.UpstreamTurnID != ""
	if err := options.Validate(); err != nil {
		return nil, nil, err
	}
	var source object
	if err := decode(raw, &source); err != nil || source == nil {
		return nil, nil, fmt.Errorf("invalid Basispoints request JSON")
	}
	if err := normalizeContent(source); err != nil {
		return nil, nil, err
	}
	model := strings.TrimSpace(text(source["model"]))
	if model == "" {
		return nil, nil, fmt.Errorf("basispoints requires a model")
	}
	if text(source["previous_response_id"]) != "" {
		return nil, nil, fmt.Errorf("basispoints requires expanded history instead of previous_response_id")
	}
	requested := text(source["reasoning_effort"])
	if reasoning, ok := source["reasoning"].(object); ok {
		requested = text(reasoning["effort"])
		if mode := text(reasoning["mode"]); mode != "" && mode != "standard" {
			return nil, nil, fmt.Errorf("basispoints does not support reasoning mode %q", mode)
		}
	}
	// Historical configuration events are metadata. The current request's
	// explicit effort is authoritative; otherwise use the last history update.
	currentEffort := requested
	if input, ok := source["input"].([]any); ok {
		for _, raw := range input {
			item, _ := raw.(object)
			if text(item["type"]) != "configuration_update" {
				continue
			}
			updated, err := historyConfigurationEffort(item)
			if err != nil {
				return nil, nil, err
			}
			if currentEffort == "" {
				requested = updated
			}
		}
	}
	effort, err := NormalizeEffort(requested)
	if err != nil {
		return nil, nil, err
	}
	structured, err := prepareStructuredOutput(source["text"])
	if err != nil {
		return nil, nil, err
	}
	b := &Bridge{RequestedEffort: requested, Effort: effort, tools: make(map[string]tool), unsupportedTools: make(map[string]bool), structured: structured, replay: replay, scope: scope}
	choice := source["tool_choice"]
	if choice != nil && text(choice) != "auto" && text(choice) != "none" {
		return nil, nil, fmt.Errorf("basispoints supports tool_choice auto or none only")
	}
	var catalog []any
	if text(choice) != "none" {
		// Each declaration batch is internally consistent. Later history updates
		// replace matching definitions, without rewriting past tool calls.
		positions := make(map[string]int)
		merge := func(value any) error {
			batch := &Bridge{tools: make(map[string]tool), unsupportedTools: make(map[string]bool)}
			entries, err := batch.collectTools(value, "")
			if err != nil {
				return err
			}
			for kind := range batch.unsupportedTools {
				b.unsupportedTools[kind] = true
			}
			for _, raw := range entries {
				entry, ok := raw.(object)
				if !ok {
					return fmt.Errorf("invalid Basispoints client tool entry")
				}
				key := text(entry["name"])
				next := batch.tools[key]
				if previous, exists := b.tools[key]; exists && (previous.Namespace != next.Namespace || previous.Name != next.Name) {
					return fmt.Errorf("ambiguous Basispoints client tool namespace")
				}
				b.tools[key] = next
				if index, exists := positions[key]; exists {
					catalog[index] = entry
				} else {
					positions[key] = len(catalog)
					catalog = append(catalog, entry)
				}
			}
			return nil
		}
		if err := merge(source["tools"]); err != nil {
			return nil, nil, err
		}
		if input, ok := source["input"].([]any); ok {
			for _, raw := range input {
				item, _ := raw.(object)
				if text(item["type"]) == "additional_tools" {
					if err := merge(item["tools"]); err != nil {
						return nil, nil, err
					}
				}
			}
		}
	}
	var input []any
	switch v := source["input"].(type) {
	case string:
		input = []any{message("user", v)}
	case []any:
		input = v
	default:
		return nil, nil, fmt.Errorf("basispoints input must be text or a Responses item array")
	}
	translated, err := b.translateHistory(input)
	if err != nil {
		return nil, nil, err
	}
	prologue := make([]any, 0, 2)
	if instructions := text(source["instructions"]); instructions != "" {
		prologue = append(prologue, message("developer", instructions))
	}
	protocol := "This request comes from an external Responses client. Return assistant text. Do not call Excel, Office, workbook or connector tools."
	if len(catalog) > 0 {
		protocol = "This request comes from an external Responses client. Use only the client tools in the catalog below. " +
			"There is no live Excel workbook for this request. The proxy intercepts run_officejs as a transport and never executes Office code. " +
			"To call one client tool, call native run_officejs using the transport matching its catalog type. " +
			"FUNCTION: code must contain one serialized JSON object {\"name\":\"CATALOG_NAME\",\"arguments\":{...}}. Arguments is an object, not an extra JSON string. " +
			"CUSTOM: set summary to exactly codex2api.custom/CATALOG_NAME and put the exact raw tool input directly in code. Do not wrap custom input in another JSON object or add Markdown fences. " +
			"For example, custom functions.exec uses summary=codex2api.custom/functions.exec and code containing its raw JavaScript; custom functions.apply_patch uses its exact patch text. The marker is mandatory for raw input. " +
			"CATALOG_NAME includes its exact namespace. Outer arguments also include extended_summary, destructive=false and references=[]. For FUNCTION transport, use an ordinary descriptive summary. " +
			"Never nest run_officejs inside code. Serialize outer native arguments with proper JSON escaping. For FUNCTION envelopes also escape all quotes, backslashes, newline, carriage return and tab characters within JSON string values. " +
			"Call one client tool at a time, including update_plan through this transport. After receiving its result continue the task; do not repeat completed calls. " +
			"Tool results replayed under run_officejs are the named client tool's results. When a tool is needed, emit its call in this response instead of only announcing it. " +
			"Do not call other native tools or claim that shell, filesystem or workspace access is unavailable when a suitable catalog tool exists. " +
			"If no tool is needed, answer as assistant text. Client tool catalog:\n" + describeCatalog(catalog) +
			"\nEnd of catalog. Invoke native run_officejs once. FUNCTION uses a JSON envelope in code. CUSTOM uses the exact codex2api.custom/CATALOG_NAME summary marker and raw input in code. No Office code is executed by the proxy."
	}
	if len(b.unsupportedTools) > 0 {
		kinds := make([]string, 0, len(b.unsupportedTools))
		for kind := range b.unsupportedTools {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		warning := "Hosted tools unavailable through Basispoints: " + strings.Join(kinds, ", ")
		b.Warnings = append(b.Warnings, warning)
		protocol += "\n" + warning + ". These declarations were omitted. Do not claim to have used them. If the task requires one, explain the limitation or use a suitable declared client tool."
	}
	if structured != nil {
		protocol += "\n" + structured.instructions()
	}
	prologue = append(prologue, message("developer", protocol))
	cacheKey := text(source["prompt_cache_key"])
	conversation := cacheKey
	if conversation == "" && len(input) > 0 {
		conversation = fingerprint(input[0])
	}
	taskID := fingerprint([]any{scope, conversation})
	sessionID := strings.TrimSpace(options.SessionID)
	if sessionID != "" {
		// Keep an explicit conversation stable when the client trims history or
		// changes prompt_cache_key. The scope still isolates accounts/callers.
		taskID = fingerprint([]any{"bps-session-v1", scope, sessionID})
	}
	turnEnd := 0
	if len(input) > 0 {
		turnEnd = 1
	}
	iteration := 1
	for i := len(input) - 1; i >= 0; i-- {
		item, _ := input[i].(object)
		if text(item["role"]) == "user" {
			turnEnd = i + 1
			break
		}
		if strings.HasSuffix(text(item["type"]), "_call_output") {
			iteration++
		}
	}
	turnID := fingerprint([]any{scope, input[:turnEnd]})
	if sessionID != "" {
		turnID = fingerprint([]any{"bps-turn-v1", taskID, input[:turnEnd]})
	}
	if hasUpstreamIdentity {
		// A leased account slot may serve multiple callers. Only these upstream
		// metadata values are shared; b.scope and all local replay keys remain
		// scoped to the original caller, authorization and logical conversation.
		taskID, turnID = options.UpstreamTaskID, options.UpstreamTurnID
	}
	output := object{
		"model": model, "model_selection": "explicit", "stream": true, "store": false,
		"input": append(prologue, translated...), "reasoning_effort": effort,
		"context_management": []any{object{"type": "compaction", "compact_threshold": 200000}},
		"metadata": object{
			"task_id": taskID,
			"turn_id": turnID, "agent_iteration": fmt.Sprint(iteration),
		},
	}
	if cacheKey != "" {
		output["prompt_cache_key"] = "bps-" + fingerprint([]any{scope, cacheKey})
	}
	if management, ok := source["context_management"].([]any); ok {
		output["context_management"] = management
	}
	body, err := json.Marshal(output)
	return body, b, err
}
