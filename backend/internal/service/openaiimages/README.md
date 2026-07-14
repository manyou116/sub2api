# OpenAI Web Images (fork)

ChatGPT **Web image quota** path for OpenAI-compatible `POST /v1/images/generations`
with `model=gpt-image-2` (and related image models), using OAuth accounts.

## Default off

- Global: `gateway.openai_web_images.enabled=false`
- Account: `extra.openai_web_images.enabled=false` until admin enables

Upstream text/Codex traffic is unchanged when disabled.

## Layout (merge-friendly)

| Path | Role |
|------|------|
| `openaiimages/webdriver/` | Reverse driver (SSE → post-SSE poll → download) |
| `../openai_web_images_service.go` | Control plane: quota, inflight, cooldown, bulk, model resolve |
| `../openai_images_legacy_web.go` | Gateway branch + text-rate-limit bypass helpers |
| `../../handler/admin/openai_web_images_handler.go` | Admin REST |

### Upstream touch points (keep small)

- `config.OpenAIWebImages` + viper defaults
- `wire` ProvideOpenAIWebImagesService
- `routes/admin` register
- Scheduler: only image requests use text-429 bypass + extra candidate list
- `forwardOpenAIImagesOAuth`: 3-line web-path branch

## Runtime contract

1. **SSE open**: no `GET /conversation/{id}` polling
2. **SSE end / idle / DONE without assets**: adaptive poll + 429 backoff
3. **Image quota** (Free plan text) → web cooldown / remaining=0  
   **HTTP 429 on conversation read** → transport throttle only (no quota burn)
4. Hide conversation after result (`is_visible=false`) unless `keep_conversation_after`
5. Prompt: force-generate + size/quality hints (`webdriver.BuildImagePrompt`)

## Recommended commit split for rebase

1. `fork(webimg): driver + control plane + config default off` (mostly new files)
2. `fork(webimg): gateway schedule + oauth images branch`
3. `fork(webimg): admin api + frontend capacity/bulk/edit`

## Local only (do not commit)

- `backend/.air.toml`, `DEV_LOCAL.md`
- Local `backend/config.yaml` secrets / `enabled: true` for dev
- `/tmp/webimg-stage.log` (`WEBIMG_DEBUG=1`)

## Ops

```yaml
gateway:
  openai_web_images:
    enabled: true
    inflight_backend: redis
    default_max_inflight: 1
    unknown_quota_policy: strict   # require probe before schedule
    probe_on_schedule: true
```

Per account (admin UI or extra JSON):

```json
{
  "openai_web_images": {
    "enabled": true,
    "max_inflight": 1,
    "model_mode": "auto"
  }
}
```

## Upstream tracking

See [`deploy/upstream-sync/README.md`](../../../../deploy/upstream-sync/README.md) and
`.github/workflows/upstream-sync.yml`.

Fork tags: `v{upstream}-webimg.{n}` (e.g. `v0.1.153-webimg.1`).
