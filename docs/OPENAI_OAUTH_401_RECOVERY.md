# Codex OAuth 401 recovery

Official HTTPS Codex `/backend-api/codex/responses` and `/responses/compact`
requests can recover an explicitly expired access token before `expires_at`.
The request uses existing bounded account failover; recovery runs asynchronously
inside `TokenRefreshService`, sharing provider rate/concurrency limits and
`OAuthRefreshAPI` locking. No dependency or migration is required.

## Operation

Default enabled: `token_refresh.openai_401_recovery_enabled: true`;
environment: `TOKEN_REFRESH_OPENAI_401_RECOVERY_ENABLED=true`.
The regular `token_refresh.enabled` switch must also enable the refresh service;
when it is disabled, new requests retain the existing HTTP error handling.
Disabling recovery stops new recovery admission/tasks; it does not automatically
release credentials already known to be invalid. Do not disable the background
refresh service while expecting automatic recovery to run.

The account's `extra.openai_oauth_recovery` document contains only state, an
access-token fingerprint, generation, attempt count and retry/lease timestamps:

- `pending`: rejected version is excluded from scheduling and token-provider use.
- `refreshing`: one version-fenced recovery attempt owns a bounded lease.
- `verify`: replacement credentials are persisted; one HTTP request may acquire
  the verification lease via database CAS. Admission rechecks current credentials
  and proxy/identity fields to fence stale scheduler and AT-cache snapshots.
- `probing`: verification is in flight. A 2xx upstream HTTP response clears only
  this authentication state; it does not prove the full SSE generation completed.
  Non-2xx/transport failures release the probe with 30s backoff. Probe headers
  have a 30s timeout; the lease expires after one minute if the process dies.
- `reauth`: revoked/missing credentials or a permanent refresh error require
  reauthorization. The account is also marked `error`.
- `attention`: unknown 401, rejected replacement, interrupted rotation,
  exhausted retries or uncertain persistence require inspection. The recovery
  gate excludes the version even if a timed cooldown has expired.

A late response cannot quarantine a newer token or clear a newer generation.
A duplicate 401 is coalesced. No-refresh-token, token_revoked/token_invalidated
and account_deactivated are isolated. Unknown 401s are not assumed permanently
revoked. Shadows resolve the credential owner; normal parent/manual scheduling,
quota, transport and image lifecycle rules remain intact.

Pending recovery is durable. A coalesced wakeup accelerates a five-second,
100-row cursor scan over the existing JSONB GIN-indexed recovery cohort. Each
upstream refresh attempt shares existing provider limits. Temporary failures
back off (30s, 60s), with at most three attempts; lock contention does not consume
a refresh attempt. An expired **rotation** lease requires inspection, because
its refresh token may already have been consumed. It is never blindly replayed.
A live process retains failed quarantine writes locally and retries persistence
in bounded batches. If storage never accepted a write, a simultaneous process
crash cannot make that local observation durable; final dispatch fails closed
when the account cannot be reread.

Credential updates compare the exact credential document, proxy and recovery
state, and write the scheduler outbox in the same SQL statement. Successful
provider rotations receive a separate bounded persistence cleanup context even
if the caller timed out. CAS conflicts reread state: administrator replacements
win; metadata-only changes preserve new metadata while retrying the already
issued token's persistence, never another RT exchange. If a provider returns the
same AT with a new RT, the RT is saved and the account remains under inspection.
An administrator stopping the account during rotation does not discard the new
credentials; its stopped status and scheduling flags remain unchanged.

Only the recovery document is cleared on successful verification. Manual pause,
other temporary cooldowns, rate-limit and quota fields are never cleared here.
Reauthorizing with a different AT makes old recovery state inapplicable; normal
scheduled token refresh continues for that new credential.

## Boundaries and recovery

This hook handles actual HTTP authentication responses on the official Codex
route, including streaming requests whose response status is 401. Plugins,
API-key/PAT/Agent Identity accounts, custom upstreams and other platforms keep
existing transport behavior. WebSocket handshakes and authentication errors
carried later inside an already accepted SSE stream retain their existing error
handling; this change does not claim to recover every authentication surface.
Existing recovery state still protects the owner from ordinary scheduling.

No model-generation health probes are sent. Verification uses a real eligible
request. Requests already producing semantic output are not replayed by this
feature. A 2xx response verifies HTTP authentication, not complete inference.

Inspect account extra phase/reason and `openai_oauth_recovery_*` log events.
For `reauth`/`attention`, update the account through the existing reauthorization
flow. Merely clearing an error or waiting for a timer does not permit the same
rejected credential back into the pool. Avoid older releases while recovery
states exist: older code does not enforce this gate. Drain or reauthorize those
accounts before a rollback, or keep them explicitly paused.

Tests cover the real service transition sequence, concurrent coalescing,
single HTTP verification admission, late response fencing, DB/Redis failures,
RT persistence retries, cancellation, restart compensation and administrator
replacement. PostgreSQL tests validate CAS/outbox behavior and preservation of
unrelated account fields.
