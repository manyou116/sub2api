package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBPSShadowUsesOrdinaryModelsAndNeverBorrowsCodex(t *testing.T) {
	const alias = "bps-example"
	const model = "gpt-6.1-sol"
	id := int64(42)
	ordinary := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	shadow := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &id, QuotaDimension: quotaDimensionBPS}
	bps := WithBPSResponses(t.Context(), model)
	require.False(t, bpsRouteMismatch(bps, shadow, model, false))
	require.True(t, supportsOpenAIRequestModel(shadow, model, false))
	_, mapped := resolveOpenAIForwardMappedModels(shadow, model, false)
	require.Equal(t, model, mapped)
	require.False(t, bpsRouteMismatch(bps, ordinary, model, false))
	require.True(t, bpsRouteMismatch(bps, shadow, "gpt-image-2", false))
	require.True(t, bpsRouteMismatch(bps, shadow, alias, false))
	require.True(t, bpsRouteMismatch(bps, ordinary, alias, false))
	require.True(t, bpsRouteMismatch(t.Context(), shadow, model, false))
	require.True(t, bpsRouteMismatch(bps, shadow, model, true))
	shadow.Credentials = map[string]any{"model_mapping": map[string]any{alias: "example", "team-*": model}}
	require.False(t, bpsRouteMismatch(bps, shadow, alias, false))
	require.True(t, supportsOpenAIRequestModel(shadow, "team-text", false))
	require.False(t, supportsOpenAIRequestModel(shadow, model, false))
	_, mapped = resolveOpenAIForwardMappedModels(shadow, "team-text", false)
	require.Equal(t, model, mapped)
	_, err := resolveCredentialAccount(t.Context(), nil, shadow)
	require.ErrorContains(t, err, "independent BPS authorization")
}

func TestBPSIdentityAndQuotaAreIndependentOfCodexCredentials(t *testing.T) {
	parent := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
	bound := bpsbridge.BoundGrant{Identity: bpsbridge.Identity{Account: "workspace", User: "user"}}
	require.True(t, bpsMatchesParent(bound, parent))
	bound.Identity.Account = "wrong"
	require.False(t, bpsMatchesParent(bound, parent))
	now := time.Now()
	require.True(t, bpsParentQuotaAllows(context.Background(), parent, "model"))
	reset := now.Add(time.Hour)
	parent.RateLimitResetAt = &reset
	require.False(t, bpsParentQuotaAllows(context.Background(), parent, "model"))
	_, ok := bpsSharedQuotaReset(gjson.Parse(`{"type":"rate_limit_exceeded","resets_at":1999999999}`), now)
	require.False(t, ok)
	_, ok = bpsSharedQuotaReset(gjson.Parse(`{"type":"usage_limit_reached"}`), now)
	require.False(t, ok)
	observed, ok := bpsSharedQuotaReset(gjson.Parse(fmt.Sprintf(`{"type":"usage_limit_reached","resets_at":%d}`, reset.Unix())), now)
	require.True(t, ok)
	require.Equal(t, reset.Unix(), observed.Unix())
}

func TestBPSStreamUsesTerminalEvent(t *testing.T) {
	const early = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"
	const completed = "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3}}}\n\n"
	var seen int
	consume := func(string, []byte) error { seen++; return nil }
	event, err := bpsReadStream(strings.NewReader(early+completed+early), 32<<10, consume)
	require.NoError(t, err)
	require.Equal(t, "response.completed", event.Get("type").String())
	require.Equal(t, int64(3), event.Get("response.usage.input_tokens").Int())
	require.Equal(t, 2, seen)
	_, err = bpsReadStream(strings.NewReader(early), 32<<10, consume)
	require.Error(t, err)
}
