# OpenAI Web Images — Final Design

ChatGPT Web `picture_v2` path for `gpt-image-2` (fork).

## Architecture

| Layer | Storage | Role |
|-------|---------|------|
| Global default | `gateway.openai_web_images.default_enabled` / `GATEWAY_OPENAI_WEB_IMAGES_DEFAULT_ENABLED` | Fleet switch |
| Account config | `accounts.extra.openai_web_images` | Override enable + max_inflight + model + stats |
| Image rate limit | columns `web_image_rate_limited_at`, `web_image_rate_limit_reset_at` | Durable gate (aligned with text `rate_limit_*`) |
| Hot quota / inflight | Redis `sub2api:webimg:*` | Multi-instance |
| Soft throttle | Redis cooldown cache only (~90s) | Must not day-pin accounts |

## Enablement

```
effective = account.extra.openai_web_images.enabled if key present
          else gateway.openai_web_images.default_enabled
```

Admin UI / API: `enabled_mode` = `inherit` | `on` | `off`.

**Import / sync must not write `enabled` unless intentional exception.**

### UpdateAccount safety

Generic `UpdateAccount` replaces whole `extra`. If request `extra` **omits** `openai_web_images`, server **preserves** the previous value (same idea as `quota_used` protection). Explicit `openai_web_images` in body still replaces that key.

Prefer control-plane:

- `PATCH /api/v1/admin/accounts/:id/openai-web-images`
- `POST /api/v1/admin/accounts/openai-web-images/bulk`

## Rate limit (aligned with text models)

| Channel | Columns | Meaning |
|---------|---------|---------|
| Text | `rate_limit_reset_at` | Text/Codex 429 window |
| Web image | `web_image_rate_limit_reset_at` | Image path only |

- Store **reset timestamp**, not “keep N days”.
- Expired when `now >= reset_at` (auto-schedulable again).
- Text 429 does **not** block web images; web image cooldown does **not** block text.
- Daily Free-plan cap → parse “resets in …” → durable pin (often ~24h, cap ~48h).
- Soft poll/CF 429 → reason `soft`, Redis-only ~90s (no durable pin).
- Stats: `last_rate_limit_reason` = `quota_daily` | `rate_limit` | `soft`.

## Quota remaining

- Redis cache `quota:{id}` (`remaining`, optional `reset_at`, `probed_at`), TTL `quota_cache_ttl_seconds`.
- Not a ledger; re-probe after reset. `unknown_quota_policy=optimistic` recommended for large pools.
- No full-fleet high-frequency probe.

## Scheduling

1. Filter `web_image_rate_limit_reset_at > now`
2. Skip text concurrency slot when web path
3. Bypass text rate-limit for web image models when configured
4. `evaluateSchedulable`: enabled, cooldown, inflight, remaining
5. `Acquire` inflight → generate → success decrement / fail classify

## Config example

```yaml
gateway:
  openai_web_images:
    default_enabled: true
    default_max_inflight: 1
    inflight_backend: redis
    rate_limit_cooldown_seconds: 900
    quota_cache_ttl_seconds: 600
    unknown_quota_policy: optimistic
    probe_on_schedule: true
```

## Admin API

- `PATCH /api/v1/admin/accounts/:id/openai-web-images`
- `GET  /api/v1/admin/accounts/:id/openai-web-images/status`
- `POST /api/v1/admin/accounts/:id/openai-web-images/probe`
- `POST /api/v1/admin/accounts/:id/openai-web-images/clear-cooldown`
- `POST /api/v1/admin/accounts/openai-web-images/bulk`
- `POST /api/v1/admin/accounts/openai-web-images/bulk-probe`
- `GET  /api/v1/admin/accounts/openai-web-images/overview?ids=`

## Rebase / fork files

See `docs/FORK_HOOKS.md`. Prefer growing `openai_web_images_*.go`, `openaiimages/**`, `account_*webimg*`.
