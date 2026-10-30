# Web and WM Image Shadows

## Approved behavior

ChatGPT image execution uses two fixed shadow purposes: `web_images` consumes the
parent's Web image quota, while `work_images` shares the parent's Codex/model quota.
Both carry `parent_account_id`. Creating a shadow explicitly enables it: active
and schedulable by default, without another enable flag. Pausing uses the existing
account scheduling switch. Failure or exhaustion never silently switches modes.

Each real OpenAI credential account can have one live Spark, one Web image and
one WM image shadow. Shadows cannot themselves be parents. Keep the existing
Spark API and behavior compatible. Validate Web-compatible parent credentials;
OAuth and setup-token compatibility must be deliberate. Resolve shared identity
from the current parent at use time, including token, ChatGPT account ID, device,
session, User-Agent, plan and proxy. Never persist parent auth credentials in a shadow.

## Scheduling and configuration

Only Web and WM image shadows enter the retained ChatGPT protocol adapter. Other
image API accounts retain their ordinary image flow. Restrict image shadows to
image requests, including supported chat-to-image requests; an editable model mapping cannot grant text access.
Default mapping is gpt-image-2. An empty-model/general model-list request must not
accidentally schedule image execution. Existing text and Spark routing stays intact.

Reuse ordinary Account.Concurrency, Priority, GroupIDs, Status and Schedulable.
Default image concurrency is 1 and must be positive. Default groups and priority
come from the parent. All image entrypoints acquire exactly one ordinary account
slot, including admin image tests. Hold it throughout uploads, generation, polling
and download; release on success, failure and cancellation. Retain user/process
limits. Capacity UI may split text and image totals by account purpose while using
the same generic concurrency store. Never count either image shadow as text
capacity or add a second inflight counter or parent-plus-children concurrency cap.

Execution type is fixed by the account's quota dimension. Model selection stays
inside that type; neither an attachment checkbox nor a model suffix can change it.
The small backend catalog at `GET /api/v1/admin/accounts/image-shadow-models` supplies
model IDs, supported modes, thinking choices and defaults to the UI. Settings use
`extra.openai_web_images` for both types to reuse the existing adapter.

The current recommended Web model is `gpt-5-6-thinking`, and WM is
`gpt-5.6-luna-wm`; both default to `standard`. The upstream directory advertises
`standard` and `extended`. The UI calls this choice “推荐模型”, while “上游自动选择”
means the actual upstream `auto` model (currently Web only). Advanced fixed model
and thinking values are validated against the catalog. A successful real WM image
confirmed the Luna WM model with `min`; the `standard` default comes from the model
directory and is not a claim that every thinking combination was separately tested.

WM requires a downloadable attachment as an explicit output constraint. A prompt
request alone does not satisfy that constraint. Web can request an attachment
without becoming WM. Use the retained webdriver's SSE/poll/upload machinery.

Prefer separate groups for the two modes. New shadows map their own explicit alias
(`gpt-image-2-web` or `gpt-image-2-work`) to the image business model. A generic
`gpt-image-2` request resolves the configured group's sole mode before quota and
health checks, so failure cannot switch resources. If a group contains both modes,
generic requests return 400 and clients must use the explicit aliases. The UI
explains that the shared group contains two quota sources; no implicit mixed-pool
failover is enabled.

## Upstream quota and health

Concurrency and logs belong to the executing shadow. Web image quota and cooldown
belong to the credential parent. WM reads and writes the same parent Codex/model
quota used by ordinary requests, including quota refresh display caches; it has no
copied WM quota state. Deleting/recreating either shadow cannot reset resources. Keep
published migration 177 byte-identical and use its parent image cooldown columns.
Use the existing Redis client for shared quota cache/probe ownership as necessary;
there is no second inflight counter. Unknown quota stays unknown; successful probe
without remaining must not fabricate a number. Ordinary image quota is independent
of the parent's text rate limits and overload-only pause. Parent inactive/deleted,
authentication and transport failures block children. A parent's schedulable=false
alone does not pause its shadow. Either shadow's schedulable=false stops its new
image work. Parent Codex exhaustion blocks ordinary Codex and matching WM requests,
while Web can run if its image quota remains. Web exhaustion does not block WM.
Explicit WM global exhaustion updates the parent's ordinary rate limit; explicit
model exhaustion updates the corresponding parent model limit. Generic WM read
throttles use the existing temporary-unschedulable state on that child.

Explicit image quota exhaustion can persist image cooldown/remaining=0. Transport,
CF/poll throttling and Work Mode quota errors must not fabricate image exhaustion.
Quota errors must identify both the image resource and exhaustion; image-related
words or content-policy rejections alone are insufficient. Transient Web state is
stored separately from durable parent image-quota cooldown and exposes a reason:
`rate_limit`, `edge_block`, or `network_error`. Genuine quota exhaustion reports
`quota_exhausted`; old cooldowns whose cause is unknown report `legacy_cooldown`.
A successful authoritative quota probe may clear only the durable cooldown version
it observed, so a late response cannot erase a newer exhaustion event.

Before submission, bounded failover may choose another account of the same mode.
Once generation is accepted, or a transport failure leaves acceptance uncertain, never submit the
generation again automatically. Continue the existing conversation's polling and
bounded download retries; if its result remains unavailable, return
`web_image_result_unconfirmed`. Client cancellation releases the ordinary slot and
does not penalize parent health. Policy and model-permission failures do not pause
accounts. Reuse the existing transport classifier for persistent proxy/network
failures; transient failures back off Web requests without suspending the parent's
other traffic. Honor upstream `Retry-After` within the request deadline.
Definitive policy or image-quota rejection in an accepted stream preserves its
400/429 reason while remaining terminal, without automatic generation failover.

Retain SSE-before-poll behavior. Poll serialization applies to individual GETs and
their minimum spacing, rather than holding the account lock during the whole
conversation wait. The driver also bounds downloads, validates image data, closes
SSE readers on cancellation and keeps auth headers restricted to ChatGPT hosts.
These are focused changes to the retained protocol driver, not a second driver.
Poll spacing is process-local; account concurrency and cooldown state remain shared.
Downloads are limited to 20 MiB and recognized PNG/JPEG/GIF/WebP headers with
dimensions at most 32768 per side and 100 million pixels. This is a format/size
guard, not full pixel decoding or a guarantee of requested output dimensions.

The local real WM result was a downloadable PNG at 1254 × 1254. Report actual
output dimensions; neither the requested size nor the attachment requirement is a
promise of exact dimensions or 4K. User billing may continue under `gpt-image-2`,
while logs distinguish the business model, actual execution model and quota source.
The successful result did not prove upstream token usage or Codex cost. Missing
usage stays unknown and must not be replaced by estimated Codex consumption.

## Lifecycle and migration

Deleting a parent cascades all shadow types. Proxy and shared identity changes apply
to all children. Parent type changes must respect all shadow requirements. Generic
credential refresh, import and auth updates must not materialize credentials on a
shadow. Separate Spark-specific usage/window logic from generic credential shadows.

Migration 247 adds `work_images`, its per-parent uniqueness index and positive
concurrency constraint. It adds no quota columns. Keep every previously published
SQL byte unchanged. Apply new migrations only in the isolated local validation
database during development. Provide an explicit, previewable conversion path for existing Web-enabled parents; effective
enablement includes account overrides and inherited global defaults. Convert old
max_inflight to shadow concurrency and preserve actual scheduling priority, groups
and Web model options. Preserve parent quota/cooldown and historical log ownership.
Do not re-enable parent Web routes after shadow deletion. Before deployment, drain
legacy Web requests, apply conversion, then switch exclusively to shadows; never
copy live inflight counters between IDs. Reject/report incompatible parents clearly.
Shadow configuration must be recoverable through a defined backup/restore path.

## Explicit conversion and backup

`POST /api/v1/admin/accounts/:id/web-image-shadow-conversion` previews by default.
Send `apply: true` only after draining the legacy deployment. When no account
`enabled` override (including the older legacy-image override) exists, supply
`legacy_default_enabled` from the old deployment. Optional `legacy_defaults`
contains its old `max_inflight` and Web model options. The preview reports the
resulting concurrency, actual account priority, groups and model options; repeated
apply returns the existing shadow. The operation never rewrites the parent,
its image cooldown, historical usage, or live concurrency counters.

Account backup nests `web_image_shadow` and `work_image_shadow` configurations
inside a selected parent only when the corresponding child is also selected. A
shadow selected without its parent is counted in `skipped_shadows`; export never silently adds unselected credentials.
Spark keeps its previous exclusion behavior. Both image backups contain no
credentials, use OpenAI group names across instances and preserve zero priority, explicit
empty groups, model mapping/options, status and the scheduling switch. Restore
groups first: a missing or ambiguous group name fails that child restoration.
The child is created paused, configured, then enabled only if the backup requested
it. A failed child update is compensated by deleting that paused partial child;
the successfully restored parent is retained and the import reports the error.
This configuration export does not replace a database backup for parent quota
and cooldown state.

## Batch administration and status reads

`POST /api/v1/admin/accounts/web-image-shadows` accepts `parent_ids` (1 to 200
positive IDs), `modes` (`["web"]` by default, `["work"]` or both), and optional
positive `concurrency` (default 1). IDs and modes are deduplicated in order. At most
four creation workers run; 20 parents with both modes can create up to 40 children.
Each parent uses the ordinary shadow lifecycle, inheriting its own priority and groups; creation does not perform an upstream probe. The response
contains `created`, `existing`, `skipped`, `failed` counts and a `results` list with
`parent_id`, `mode`, optional `shadow_id`, `status`, optional `code` and `message`.
Existing children are reported without changing their configuration or resuming
paused accounts. Incompatible parents are skipped; independent failures do not
roll back successful children. The database uniqueness constraint remains the
final guard against concurrent creation. Uniqueness uses parent ID and purpose,
not names. Default names are `父账号名称 · Web 生图` and `父账号名称 · WM 生图`;
only the parent-name portion is truncated when needed. A child can be renamed
independently, and later parent renames never overwrite it.

The accounts UI captures all selected IDs, including selections across pages,
and sends sequential batches of at most 200 parents per type. It shows per-parent,
per-type outcomes and retries only failed pairs. The confirmation shows quota
sources and the maximum child count. Network failures leave the affected entries
retryable; repeating creation safely discovers any children created before the response was
lost. Existing and skipped results are not retried automatically.

`POST /api/v1/admin/accounts/web-image/status` accepts `account_ids` (1 to 200)
and returns `statuses` and `errors` maps keyed by shadow ID. This reads local
parent state only; explicit per-account `/web-image/probe` remains the upstream
operation. WM uses the existing `/accounts/:id/usage` and quota query/refresh paths,
resolved to the parent resource; it never calls the Web image probe. Mounted Web
quota cells coalesce reads, share pending requests for the same ID and avoid keeping their own long-lived quota cache. Candidate scheduling reads
parent state in batches, then rechecks the selected account's current state before
execution. HTTP transport reuse must keep credentials and proxy identity isolated.

## Verification

Test coexistence/uniqueness, parent deletion/type/proxy handling, image-only routing,
one-slot acquisition/release and saturation, parent credential resolution and
unhealthy states, independent Web/Codex resources, typed mode routing, mixed-group
alias requirements, explicit WM attachments, unknown quota, recreation preserving cooldown, conversion idempotency and UI creation/edit/pause.
Run affected unit tests, API JSON contracts if DTO fixtures change, backend build,
frontend typecheck/build, fork hooks and whitespace checks. Use mock protocol calls
for fault and concurrency validation. Any authorized real-provider probe must
record what it actually proves separately from model directory and quota evidence.

## Commit boundaries

1. Clean fork baseline: non-Web fork changes replayed on v0.2.16, legacy Web control
   plane removed, protocol driver preserved byte-for-byte, published migrations kept.
2. Shadow model/lifecycle and additive migration.
3. Web execution using generic concurrency and parent resource state.
4. Administration/UI plus focused integration and contract validation.
5. Protocol reliability, resource-scoped error handling and batch administration.
6. Typed WM lifecycle/resource routing and the corresponding catalog/admin UI.

Implementation agents start with fresh conversation context and this contract.
Legacy control-plane code lives only on the backup branch. Keep helper boundaries
small; avoid generic frameworks, duplicate state and speculative configuration.
