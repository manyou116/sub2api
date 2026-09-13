package service

import "time"

// Fork-owned Account helpers for ChatGPT Web image scheduling / cooldown.
// Struct fields remain on Account in account.go (tiny, stable surface).

// IsSchedulableIgnoringTextRateLimit is used for ChatGPT Web image scheduling.
// Text/Codex rate-limit windows (RateLimitResetAt/OverloadUntil) do not block web image
// generation when the account is otherwise healthy and web image path is enabled.
// Durable web-image cooldown (WebImageRateLimitResetAt) still blocks.
func (a *Account) IsSchedulableIgnoringTextRateLimit() bool {
	if a == nil || !a.IsActive() || !a.Schedulable {
		return false
	}
	now := time.Now()
	if a.AutoPauseOnExpired && a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
		return false
	}
	// Keep auth/transport temp blocks; skip text rate-limit and overload windows.
	if a.TempUnschedulableUntil != nil && now.Before(*a.TempUnschedulableUntil) {
		return false
	}
	// Web image cooldown is independent of text RateLimitResetAt.
	if a.WebImageRateLimitResetAt != nil && now.Before(*a.WebImageRateLimitResetAt) {
		return false
	}
	if a.IsAPIKeyOrBedrock() && a.IsQuotaExceeded() {
		return false
	}
	return true
}

// IsWebImageRateLimited reports durable ChatGPT Web image cooldown from DB fields.
func (a *Account) IsWebImageRateLimited() bool {
	if a == nil || a.WebImageRateLimitResetAt == nil {
		return false
	}
	return time.Now().Before(*a.WebImageRateLimitResetAt)
}
