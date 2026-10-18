package service

import (
	"net/http"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	OpenAIAutoResetCreditEnabledExtraKey     = "auto_reset_credit_enabled" // legacy: migrate to 7d only
	OpenAIAutoResetCredit5hEnabledExtraKey   = "auto_reset_credit_5h_enabled"
	OpenAIAutoResetCredit7dEnabledExtraKey   = "auto_reset_credit_7d_enabled"
	OpenAIAutoResetCredit5hThresholdExtraKey = "auto_reset_credit_5h_threshold"
	OpenAIAutoResetCredit7dThresholdExtraKey = "auto_reset_credit_7d_threshold"
	OpenAIAutoResetCreditStateExtraKey       = "codex_auto_reset_credit_state"
)

// Each window must be explicitly enabled; legacy settings only opt in to the 7d window.
type OpenAIAutoResetCreditConfig struct {
	Enabled     bool
	Enabled5h   bool
	Enabled7d   bool
	Threshold5h float64
	Threshold7d float64
}

func ResolveOpenAIAutoResetCreditConfig(account *Account) OpenAIAutoResetCreditConfig {
	config := OpenAIAutoResetCreditConfig{Threshold5h: 1, Threshold7d: 1}
	if !isOpenAIAutoResetCreditAccount(account) || account.Extra == nil {
		return config
	}
	config.Enabled5h = resolveAccountExtraBool(account.Extra, OpenAIAutoResetCredit5hEnabledExtraKey)
	if value, ok := account.Extra[OpenAIAutoResetCredit7dEnabledExtraKey]; ok {
		config.Enabled7d, _ = value.(bool)
	} else {
		config.Enabled7d = resolveAccountExtraBool(account.Extra, OpenAIAutoResetCreditEnabledExtraKey)
	}
	config.Enabled = config.Enabled5h || config.Enabled7d
	if value, ok := resolveAccountExtraNumber(account.Extra, OpenAIAutoResetCredit5hThresholdExtraKey); ok && isValidOpenAIAutoResetThreshold(value) {
		config.Threshold5h = value
	}
	if value, ok := resolveAccountExtraNumber(account.Extra, OpenAIAutoResetCredit7dThresholdExtraKey); ok && isValidOpenAIAutoResetThreshold(value) {
		config.Threshold7d = value
	}
	return config
}

func isOpenAIAutoResetCreditAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth && !account.IsShadow()
}

func isValidOpenAIAutoResetThreshold(value float64) bool {
	return value >= 0.001 && value <= 1
}

func hasOpenAIAutoResetCreditConfig(extra map[string]any) bool {
	for _, key := range []string{OpenAIAutoResetCreditEnabledExtraKey, OpenAIAutoResetCredit5hEnabledExtraKey,
		OpenAIAutoResetCredit7dEnabledExtraKey, OpenAIAutoResetCredit5hThresholdExtraKey, OpenAIAutoResetCredit7dThresholdExtraKey} {
		if _, ok := extra[key]; ok {
			return true
		}
	}
	return false
}

// Preserve absent keys so the same validation supports partial bulk updates.
func normalizeOpenAIAutoResetCreditExtra(platform, accountType string, isShadow bool, extra map[string]any) (map[string]any, error) {
	if extra == nil {
		return nil, nil
	}
	normalized := cloneOpenAIAutoResetExtra(extra)
	delete(normalized, OpenAIAutoResetCreditStateExtraKey)
	keys := []string{OpenAIAutoResetCredit5hEnabledExtraKey, OpenAIAutoResetCredit7dEnabledExtraKey}
	_, legacy := normalized[OpenAIAutoResetCreditEnabledExtraKey]
	if hasOpenAIAutoResetCreditConfig(normalized) && (platform != PlatformOpenAI || accountType != AccountTypeOAuth || isShadow) {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_AUTO_RESET_CREDIT_ACCOUNT_INVALID", "automatic reset credits are only supported for OpenAI OAuth parent accounts")
	}
	for _, key := range []string{OpenAIAutoResetCredit5hThresholdExtraKey, OpenAIAutoResetCredit7dThresholdExtraKey} {
		if _, present := normalized[key]; !present {
			continue
		}
		value, ok := resolveAccountExtraNumber(normalized, key)
		if !ok || !isValidOpenAIAutoResetThreshold(value) {
			return nil, infraerrors.Newf(http.StatusBadRequest, "OPENAI_AUTO_RESET_CREDIT_THRESHOLD_INVALID", "%s must be between 0.001 and 1.0", key)
		}
		normalized[key] = value
	}
	for _, key := range keys {
		if value, ok := normalized[key]; ok {
			if _, valid := value.(bool); !valid {
				return nil, infraerrors.Newf(http.StatusBadRequest, "OPENAI_AUTO_RESET_CREDIT_ENABLED_INVALID", "%s must be a boolean", key)
			}
		}
	}
	if legacy {
		if _, valid := normalized[OpenAIAutoResetCreditEnabledExtraKey].(bool); !valid {
			return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_AUTO_RESET_CREDIT_ENABLED_INVALID", "auto_reset_credit_enabled must be a boolean")
		}
		if _, explicit := normalized[OpenAIAutoResetCredit7dEnabledExtraKey]; !explicit {
			normalized[OpenAIAutoResetCredit7dEnabledExtraKey] = normalized[OpenAIAutoResetCreditEnabledExtraKey]
		}
	}
	delete(normalized, OpenAIAutoResetCreditEnabledExtraKey)
	return normalized, nil
}

func stripOpenAIAutoResetCreditManagedExtra(extra map[string]any, stripConfig bool) map[string]any {
	if extra == nil {
		return nil
	}
	delete(extra, OpenAIAutoResetCreditStateExtraKey)
	if stripConfig {
		delete(extra, OpenAIAutoResetCreditEnabledExtraKey)
		delete(extra, OpenAIAutoResetCredit5hEnabledExtraKey)
		delete(extra, OpenAIAutoResetCredit7dEnabledExtraKey)
		delete(extra, OpenAIAutoResetCredit5hThresholdExtraKey)
		delete(extra, OpenAIAutoResetCredit7dThresholdExtraKey)
	}
	return extra
}

func cloneOpenAIAutoResetExtra(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
