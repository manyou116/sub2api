# BPS text shadows

BPS is an optional, Responses SSE-only upstream. A BPS shadow is linked to a
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
attempt are unsupported. The bridge emits SSE; Sub2API can return the final
response object to non-streaming clients.

## Deployment

Install a trusted standalone [bps-local](https://github.com/manyou116/bps-local-public/tree/8d9e164571eea63cc322717337de82f8dce6e219)
binary alongside Sub2API. The Sub2API image does not redistribute it. Set both:

- `SUB2API_BPS_BINARY`: absolute path to the executable (read-only mount in Docker).
- `SUB2API_BPS_STATE_DIR`: absolute path on a dedicated, writable, private
  persistent volume owned by the Sub2API process (mode 0700).

This version supports a **single Sub2API replica** for BPS use. Authorization
sessions are local to the process, and task slots are coordinated with local
bridge processes. Do not use a shared state directory across active replicas
or expose the workers' ephemeral loopback listeners. Run the binary only in
an isolated container or network namespace. Sub2API uses the parent's outbound
proxy for BPS; preserve that configuration during authorization and execution.

## Authorization

From the parent account menu, choose **Create BPS shadow** and either paste
a fresh JSON file exported by `bps-local --login` or select
device-code authorization in the same dialog. Device codes must only be entered on the
provider page. The server verifies the BPS access token against OpenAI signing
keys and checks both ChatGPT user and workspace against the parent. It checks
that the bridge can start without sending a generation request. Upstream model
permission is checked by BPS when the model is used. Failed or canceled
authorization leaves a newly created shadow pending and does not replace an
existing grant.

The bridge's credentials live only on its private state volume (files mode
0600). They are not included in account data export or admin API responses.
Back up that volume as a secret, separately from the account configuration.
The account menu shows the BPS expiry; this version uses manual reauthorization,
not automatic refresh. Account or parent deletion removes the binding;
re-authorizing never updates a Codex token. No live integration check was
performed using a real grant during development.

The shadow uses a normal account concurrency slot and owns its usage logs.
A confirmed shared `usage_limit_reached` with an explicit reset timestamp
updates the parent quota. A generic 429, auth or transport error does not.
Once a request is sent to the bridge, Sub2API never resubmits it to any account.
