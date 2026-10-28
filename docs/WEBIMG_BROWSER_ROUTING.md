# Web Images Browser Routing

## Capture Evidence

The user-supplied successful HAR from 2026-10-15 contains a formal
`POST /backend-api/f/conversation` with `model: auto`, no `thinking_effort`,
no `conversation_mode`, and `client_prepare_state: success`. Its SSE contains
`model_slug: gpt-6` and `default_model_slug: auto`. File metadata and PNG download
requests succeed. These observations establish a working request shape; they
do not establish that any single omitted field caused other accounts' 404s.

This capture is from Codex Browser: `originator: Codex Browser`,
`x-openai-web-frontend: codex_webview`, `x-openai-codex-window-type: browser`,
and `client_contextual_info.app_surface: codex_browser`. It includes an account
header and a device header. The visible conversation prepare request occurs
after the formal request and image download. It is not evidence that its token
prepared the earlier request.

The committed test fixture contains only selected public protocol constants.
Prompts, account identifiers, tokens, cookies, signed download URLs, conversation
identifiers, and device identifiers from the HAR are not included.

## Corrected Behavior

| Area | Behavior |
| --- | --- |
| Automatic model selection | Send `auto` unchanged and omit reasoning effort so the upstream selects both. Never substitute the observed response model. |
| Explicit model | Preserve the configured GPT slug, reasoning effort, and existing work-mode fields. |
| Conversation mode | Omit the previously forced `primary_assistant` selector in prepare and formal requests. |
| Preparation | Use `sent` while preparing. Use `success` in the formal request only when this request received a conduit token. Without a token, use `none` and omit the token header. |
| Request identity | Generate a new parent UUID per operation and share it and the turn trace between prepare and formal requests. |
| Account context | Use the selected account's current token, ChatGPT account ID, configured User-Agent, device ID and session ID for probe, generation and edit. Never borrow another account's context. |
| Missing device ID | Derive a stable per-account UUID; never copy a captured device ID. |
| Client context | Remove stale hardcoded build/version headers, browser client hints and fabricated viewport metrics. Ordinary web requests remain the default. |
| Sentinel finalize | Use `proofofwork` and `turnstile` field names, with this request's generated proof. Do not replay captured challenge responses. |
| Billing | Keep `gpt-image-2` as the requested/billing model and `auto` as the sent upstream model. |

An account can explicitly select the captured client surface through the
top-level account extra field below. This option defaults to off and requires
no migration. It survives updates to the nested Web Images configuration.

```json
{"openai_web_images_client_profile": "codex_browser"}
```

With this profile, the three Codex Browser routing headers and matching
`app_surface` are sent together. This does not implement browser-local tools:
`local.continue_in_work` is deliberately not advertised. The driver also does
not claim support for the capture's compressed SSE extension.

## Timezone

Web Images uses `America/New_York` by default. Set the dedicated environment
variable to the IANA timezone for the actual egress, for example:

```yaml
environment:
  GATEWAY_OPENAI_WEB_IMAGES_TIMEZONE: America/New_York
  # Alternatives: America/Los_Angeles, UTC, Asia/Shanghai
```

This controls quota probe, conversation prepare, generation/edit and Sentinel
date fingerprints together. Offsets use the request date and timezone database,
including daylight saving time, with JavaScript's UTC-minus-local sign. An
invalid value fails the Web Images operation before any upstream request.
Changing the container's `TZ` alone does not override this setting. The setting
does not alter text/Codex requests, application timestamps or billing.

No external IP geolocation lookup is performed. A country alone cannot select
a timezone for countries with multiple zones, and different bound proxies may
have different egress. Configure the actual egress timezone deliberately.

## Validation Boundary

Regression tests inspect the final outbound JSON and headers for generation,
edit and quota probe, including ordinary/Codex Browser contexts, explicit
models, token/no-token prepare results, fresh parent and trace linkage, image
download, and existing image billing/logging. A synthetic SSE with an observed
`model_slug` tests parsing without turning that value into a routing rule.

These offline tests verify the emitted protocol and response handling. They
are not live single-variable experiments against ChatGPT and do not guarantee
that the same account, subscription, proxy or client surface receives the same
model. Production deployment and live acceptance must be reported separately.

On 2026-10-16 (Asia/Shanghai), a bounded comparison on the previously authorized
account used its current production credential, the same server egress,
ordinary web profile, prompt and `model: auto`. UTC sent offset `0` and a
`GMT+0000` Sentinel date; New York sent offset `240` and `GMT-0400`. Both reached
quota probe, Sentinel prepare/finalize and conversation prepare with HTTP 200,
then received HTTP 404 `Model not found` at the formal conversation request.
These results verify timezone propagation, but do not establish that UTC works
or that timezone alignment alone resolves the 404. No production settings or
stored credentials were changed by the comparison.
