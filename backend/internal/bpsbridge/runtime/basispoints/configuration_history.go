package basispoints

import "fmt"

// Only known reasoning metadata can be consumed locally. Other configuration
// changes need explicit translation rather than being silently discarded.
func historyConfigurationEffort(item object) (string, error) {
	for key := range item {
		if key != "type" && key != "reasoning" {
			return "", fmt.Errorf("unsupported history configuration field")
		}
	}
	reasoning, ok := item["reasoning"].(object)
	if !ok {
		return "", fmt.Errorf("history configuration requires reasoning")
	}
	for key := range reasoning {
		if key != "effort" && key != "mode" {
			return "", fmt.Errorf("unsupported history reasoning field")
		}
	}
	if mode, exists := reasoning["mode"]; exists && mode != "standard" {
		return "", fmt.Errorf("unsupported history reasoning mode")
	}
	effort, ok := reasoning["effort"].(string)
	if !ok || effort == "" {
		return "", fmt.Errorf("history configuration requires reasoning effort")
	}
	return NormalizeEffort(effort)
}
