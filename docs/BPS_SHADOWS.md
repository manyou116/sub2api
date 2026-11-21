# BPS text shadows

BPS is an optional, Responses SSE upstream. A BPS shadow is linked to a
real OpenAI OAuth account, but never obtains its Codex credentials. Each parent
can have one BPS shadow alongside its existing Spark and image shadows. Creation
inherits the parent account's groups; there is no group selector. If the parent
has no groups, the shadow uses an existing active `openai-default` group if
available. Otherwise it can still be created and authorized, but will not be
selected by grouped API-key traffic until a group is assigned.
Model selection and mapping use the ordinary Sub2API configuration, with no model
required at creation. Existing explicit `bps-<model>` mappings stay valid and can
be edited in the account model mapping. Send full history. Compact,
previous-response state, hosted tools, and resubmission to Codex after a BPS
attempt are unsupported. The runtime emits SSE; Sub2API can return the final
response object to non-streaming clients. Standard `POST /v1/chat/completions`
requests with `messages` and optional function tools also use the BPS runtime.
Sub2API converts the request to Responses and returns Chat Completions JSON
or Chat SSE according to the client's `stream` value.

## Deployment

Sub2API runs the BPS service in-process. The normal Sub2API binary and
image include the runtime. Deployments that previously set
`SUB2API_BPS_BINARY` can remove the variable and executable mount.

The default private state directory is `${DATA_DIR}/bps` or `./data/bps`
when `DATA_DIR` is unset. The default is `/app/data/bps` in the official
container. Keep an existing absolute `SUB2API_BPS_STATE_DIR` and its persistent
volume to retain authorized shadows and task keys across upgrades. The BPS
state root and per-shadow directories must be private (0700).

Run BPS on a single active replica. Authorization sessions and task admission
are local to each Sub2API process. Sub2API uses the parent's outbound proxy
for device authorization and generation. The runtime's upstream transport is
WebSocket, with one upstream request per Responses submission.

### Experimental WebSocket reuse

In the BPS shadow's edit dialog, turn on **Reuse WebSocket connections
(experimental)**. The setting is stored as `extra.bps_ws_session_reuse` and
defaults to off. A request needs an explicit session
header and a source-user identity supplied by an authenticated, bound
gateway. The gateway must overwrite attribution headers from clients.
Requests without both signals use a fresh socket.

Reuse stays within the same API key, source user, session, mapped model,
shadow, upstream task slot, grant and proxy. Requests still include full
history. After a clean turn, the worker can retain one idle socket for
30 seconds, up to 64 idle sockets per instance. Each request still acquires
its normal concurrency slot. An uncertain submission is never replayed.

The `bps.ws_turn` debug log includes the reuse result, first-text time,
total time and completion status. A live `gpt-6-luna` test through the embedded
runtime confirmed that two consecutive turns completed with distinct response
IDs and one handshake.

## Authorization

From the parent account menu, choose **Create BPS shadow** and either paste
a fresh JSON file exported by `bps-local --login` or select
device-code authorization in the same dialog. Device codes must only be entered on the
provider page. The server verifies the BPS access token against OpenAI signing
keys and checks both ChatGPT user and workspace against the parent. It checks
that the embedded runtime can start without sending a generation request. Upstream model
permission is checked by BPS when the model is used. Failed or canceled
authorization leaves a newly created shadow pending and does not replace an
existing grant.

BPS credentials live only on the private state volume (files mode
0600). They are not included in account data export or admin API responses.
Back up that volume as a secret, separately from the account configuration.
The account menu shows the BPS expiry; this version uses manual reauthorization,
not automatic refresh. Account or parent deletion removes the binding;
re-authorizing never updates a Codex token.

The shadow uses a normal account concurrency slot and owns its usage logs.
A confirmed shared `usage_limit_reached` with an explicit reset timestamp
updates the parent quota. A generic 429, auth or transport error does not.
Once a request is sent to the BPS runtime, Sub2API never resubmits it to any account.
