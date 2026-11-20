package basispoints

import "fmt"

// Normalize client image representations without dropping content or inventing URLs.
// This runs before image relay storage, and again in Prepare for HTTPS-only callers.
func normalizeContent(source object) error {
	input, _ := source["input"].([]any)
	for _, raw := range input {
		item, _ := raw.(object)
		for _, field := range []string{"content", "output"} {
			if field == "output" && text(item["type"]) != "function_call_output" && text(item["type"]) != "custom_tool_call_output" {
				continue
			}
			parts, ok := item[field].([]any)
			if !ok {
				continue
			}
			for i, rawPart := range parts {
				if value, ok := rawPart.(string); ok {
					parts[i] = object{"type": "input_text", "text": value}
					continue
				}
				part, ok := rawPart.(object)
				if !ok {
					continue
				}
				kind := text(part["type"])
				if kind != "image" && kind != "image_url" && kind != "input_image" {
					continue
				}
				result := object{"type": "input_image"}
				if detail, exists := part["detail"]; exists {
					result["detail"] = detail
				}
				if fileID, exists := part["file_id"]; exists {
					result["file_id"] = fileID
				}
				switch kind {
				case "image_url", "input_image":
					switch v := part["image_url"].(type) {
					case string:
						result["image_url"] = v
					case object:
						result["image_url"] = v["url"]
						if detail, exists := v["detail"]; exists {
							result["detail"] = detail
						}
					}
				case "image":
					if source, ok := part["source"].(object); ok {
						switch text(source["type"]) {
						case "base64":
							if text(source["media_type"]) == "" || text(source["data"]) == "" {
								return fmt.Errorf("basispoints image source requires media_type and data")
							}
							result["image_url"] = "data:" + text(source["media_type"]) + ";base64," + text(source["data"])
						case "url":
							result["image_url"] = source["url"]
						default:
							return fmt.Errorf("basispoints image source supports base64 or url only")
						}
					} else if text(part["mimeType"]) != "" && text(part["data"]) != "" {
						result["image_url"] = "data:" + text(part["mimeType"]) + ";base64," + text(part["data"])
					} else {
						return fmt.Errorf("basispoints image requires a source or MCP mimeType and data")
					}
				}
				if text(result["image_url"]) == "" {
					return fmt.Errorf("basispoints image requires image_url; file IDs are unsupported")
				}
				parts[i] = result
			}
		}
	}
	return nil
}

// Only fixed categories may appear in diagnostics; never echo arbitrary content.
func contentCategory(part object) string {
	switch text(part["type"]) {
	case "image", "image_url", "input_image", "input_audio", "audio", "input_file", "file", "resource", "resource_link", "output_image", "video", "encrypted_content":
		return text(part["type"])
	case "":
		return "missing_type"
	default:
		return "unknown_type"
	}
}

// Agent messages carry opaque protocol state alongside visible content. Preserve
// both in their original order; encrypted state must never become prompt text.
func validateAgentMessageContent(value any) error {
	parts, ok := value.([]any)
	if !ok {
		return fmt.Errorf("basispoints agent_message content must be an array")
	}
	for _, raw := range parts {
		part, _ := raw.(object)
		if text(part["type"]) == "encrypted_content" {
			encrypted, ok := part["encrypted_content"].(string)
			if !ok || encrypted == "" {
				return fmt.Errorf("basispoints agent_message encrypted_content must be a nonempty string")
			}
			continue
		}
		if err := validateHistoryContent([]any{raw}); err != nil {
			return err
		}
	}
	return nil
}
