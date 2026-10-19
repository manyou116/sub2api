package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const openAICapacityCooldownSettingKey = "openai_capacity_cooldown_settings"

// OpenAICapacityCooldownSettings is independent of quota/429 cooldowns.
type OpenAICapacityCooldownSettings struct {
	Enabled                 bool `json:"enabled"`
	SwitchAccountOnOverload bool `json:"switch_account_on_overload"`
	WindowSeconds           int  `json:"window_seconds"`
	FailureThreshold        int  `json:"failure_threshold"`
	FailureRatePercent      int  `json:"failure_rate_percent"`
	CooldownSeconds         int  `json:"cooldown_seconds"`
	MaxCooldownSeconds      int  `json:"max_cooldown_seconds"`
}

func DefaultOpenAICapacityCooldownSettings() OpenAICapacityCooldownSettings {
	return OpenAICapacityCooldownSettings{SwitchAccountOnOverload: true, WindowSeconds: 60, FailureThreshold: 3, FailureRatePercent: 50, CooldownSeconds: 60, MaxCooldownSeconds: 600}
}

func (v OpenAICapacityCooldownSettings) Validate() error {
	if v.WindowSeconds < 1 || v.WindowSeconds > 300 || v.FailureThreshold < 2 || v.FailureThreshold > 100 ||
		v.FailureRatePercent < 1 || v.FailureRatePercent > 100 || v.CooldownSeconds < 1 ||
		v.MaxCooldownSeconds < v.CooldownSeconds || v.MaxCooldownSeconds > 7200 {
		return errors.New("invalid capacity cooldown: window 1-300s, threshold 2-100, rate 1-100%, cooldown 1-max, max <=7200s")
	}
	return nil
}

type cachedOpenAICapacityCooldownSettings struct {
	value OpenAICapacityCooldownSettings
	until time.Time
}

func (s *SettingService) GetOpenAICapacityCooldownSettings(ctx context.Context) (OpenAICapacityCooldownSettings, error) {
	defaults := DefaultOpenAICapacityCooldownSettings()
	if s == nil || s.settingRepo == nil {
		return defaults, nil
	}
	if v, ok := s.openAICapacityCooldownCache.Load().(*cachedOpenAICapacityCooldownSettings); ok && time.Now().Before(v.until) {
		return v.value, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, openAICapacityCooldownSettingKey)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return defaults, err
	}
	v := defaults
	if strings.TrimSpace(raw) != "" {
		if json.Unmarshal([]byte(raw), &v) != nil || v.Validate() != nil {
			v = defaults // A corrupt config must not enable a new scheduling policy.
		}
	}
	s.openAICapacityCooldownCache.Store(&cachedOpenAICapacityCooldownSettings{v, time.Now().Add(30 * time.Second)})
	return v, nil
}

func (s *SettingService) SetOpenAICapacityCooldownSettings(ctx context.Context, v OpenAICapacityCooldownSettings) error {
	if err := v.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = s.settingRepo.Set(ctx, openAICapacityCooldownSettingKey, string(raw)); err != nil {
		return err
	}
	s.openAICapacityCooldownCache.Store(&cachedOpenAICapacityCooldownSettings{v, time.Now().Add(30 * time.Second)})
	return nil
}
