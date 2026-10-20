# Web Image Shadows

## Approved behavior

ChatGPT Web images are provided by a dedicated account with
`parent_account_id` and `quota_dimension = web_images`. Creating that shadow is
explicit enablement: active and schedulable by default. There is no global or
per-account Web enable flag. Pausing uses the existing account schedulable switch.
A paused/exhausted Web shadow must never fall back to another image protocol.

Each real OpenAI credential account can have one active Spark shadow and one
active Web image shadow. Shadows cannot themselves be parents. Keep the existing
Spark API and behavior compatible. Validate Web-compatible parent credentials;
OAuth and setup-token compatibility must be deliberate. Resolve shared identity
from the current parent at use time, including token, ChatGPT account ID, device,
session, User-Agent, plan and proxy. Never persist parent auth credentials in a shadow.

## Scheduling and configuration

Only Web shadows enter the ChatGPT Web protocol. Other image API accounts retain
their ordinary image flow. Restrict Web shadows to image requests, including
supported chat-to-image requests; an editable model mapping cannot grant text access.
Default mapping is gpt-image-2. An empty-model/general model-list request must not
accidentally schedule image execution. Existing text and Spark routing stays intact.

Reuse ordinary Account.Concurrency, Priority, GroupIDs, Status and Schedulable.
Default image concurrency is 1 and must be positive. Default groups and priority
come from the parent. All image entrypoints acquire exactly one ordinary account
slot, including admin image tests. Hold it throughout uploads, generation, polling
and download; release on success, failure and cancellation. Retain user/process
limits. Capacity UI may split text and image totals by account purpose while using
the same generic concurrency store. Never count a Web shadow as text capacity.

Only Web-specific model selection, thinking effort and download-attachment options
belong in extra.openai_web_images. Use the retained webdriver as a protocol adapter;
keep protocol defaults compatible and do not duplicate its SSE/poll/upload machinery.

## Upstream quota and health

Concurrency and logs belong to the shadow. Upstream image quota and cooldown belong
to the credential parent, so deleting/recreating a shadow cannot reset them. Keep
published migration 177 byte-identical and use its parent image cooldown columns.
Use the existing Redis client for shared quota cache/probe ownership as necessary;
there is no second inflight counter. Unknown quota stays unknown; successful probe
without remaining must not fabricate a number. Ordinary image quota is independent
of the parent's text rate limits and overload-only pause. Parent inactive/deleted,
authentication and transport failures block children. A parent's schedulable=false
alone does not pause its shadow. Web shadow schedulable=false stops new image work.
Work Mode (-wm) still checks the parent/model text quota and runtime restrictions.

Free-plan image quota exhaustion can persist image cooldown/remaining=0. Transport,
CF/poll throttling and Work Mode quota errors must not fabricate image exhaustion.
Preserve retry deadline, bounded failover and driver poll serialization. Never retry
an already completed generation merely to recover a response serialization failure.

## Lifecycle and migration

Deleting a parent cascades all shadow types. Proxy and shared identity changes apply
to all children. Parent type changes must respect all shadow requirements. Generic
credential refresh, import and auth updates must not materialize credentials on a
shadow. Separate Spark-specific usage/window logic from generic credential shadows.

Schema migration is additive, preserving every previously published SQL byte.
Existing local/production data is not modified during development. Provide an
explicit, previewable conversion path for existing Web-enabled parents; effective
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

Account backup nests a `web_image_shadow` configuration inside a selected parent
only when that Web shadow is also selected. A shadow selected without its parent
is counted in `skipped_shadows`; export never silently adds unselected credentials.
Spark keeps its previous exclusion behavior. Web backup contains no credentials,
uses OpenAI group names across instances, and preserves zero priority, explicit
empty groups, model mapping/options, status and the scheduling switch. Restore
groups first: a missing or ambiguous group name fails that child restoration.
The child is created paused, configured, then enabled only if the backup requested
it. A failed child update is compensated by deleting that paused partial child;
the successfully restored parent is retained and the import reports the error.
This configuration export does not replace a database backup for parent quota
and cooldown state.

## Verification

Test coexistence/uniqueness, parent deletion/type/proxy handling, image-only routing,
one-slot acquisition/release and saturation, parent credential resolution and
unhealthy states, independent image/text quotas, Work Mode checks, unknown quota,
recreation preserving cooldown, conversion idempotency and UI creation/edit/pause.
Run affected unit tests, API JSON contracts if DTO fixtures change, backend build,
frontend typecheck/build, fork hooks and whitespace checks. Use mock protocol calls
for local validation; do not consume production image quota.

## Commit boundaries

1. Clean fork baseline: non-Web fork changes replayed on v0.2.16, legacy Web control
   plane removed, protocol driver preserved byte-for-byte, published migrations kept.
2. Shadow model/lifecycle and additive migration.
3. Web execution using generic concurrency and parent resource state.
4. Administration/UI plus focused integration and contract validation.

Implementation agents start with fresh conversation context and this contract.
Legacy control-plane code lives only on the backup branch. Keep helper boundaries
small; avoid generic frameworks, duplicate state and speculative configuration.
