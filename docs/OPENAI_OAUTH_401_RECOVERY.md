# Codex OAuth expired-token 401 recovery

Default enabled: `token_refresh.openai_401_recovery_enabled: true`;
environment: `TOKEN_REFRESH_OPENAI_401_RECOVERY_ENABLED`. The existing
`token_refresh.enabled` switch must also be enabled.

For official HTTPS Codex HTTP `/backend-api/codex/responses` and
`/backend-api/codex/responses/compact`, an explicit `token_expired` or the observed
expired-token message marks the **actual sent AT** expired locally. A conditional
SQL `jsonb_set` changes only `credentials.expires_at`, never a stale copy of the
credential document. A late 401 cannot overwrite a newly rotated AT or RT.
The same statement adds a scheduling outbox event and a ten-minute auth pause;
an unrelated existing pause and manual scheduling flags remain intact.

Token-cache invalidation and a bounded account-ID notification wake the existing
`TokenRefreshService`. The ordinary refresher notices the expired timestamp and
uses its existing locking, rate/concurrency limits, retries and provider breaker.
Notifications run in the same refresh loop, including at ordinary page boundaries.
A full/lost notification is compensated by the normal expiry scan (default every
five minutes); no separate recovery worker, scan or probe state machine exists.

`extra.openai_oauth_401` contains only a token fingerprint, count and window start.
It excludes the rejected AT from scheduling/token-provider use even if a pause
expires or refreshing fails. New ATs no longer match that fingerprint. Successful
background refresh clears only this feature's auth pause; it never clears a quota
or manual pause. The fingerprint remains for counting: three different rejected
AT versions in ten minutes mark the account `error` for inspection/reauthorization.
Duplicate 401s for one AT are coalesced. Redis lock errors do not allow an unlocked
exchange of a known rejected token. No credentials or request body are logged.

Unknown/revoked 401s, missing RT, PAT/Agent Identity, plugins, custom upstreams,
WebSocket handshakes and errors inside accepted SSE keep existing error handling.
HTTP streaming requests whose status is 401 are covered. No model health probes
or automatic replay of an accepted request are added. Regular OAuth persistence
and the existing failure policy remain in use; this feature does not replace the
general refresh pipeline or guarantee a single verification request after rotation.
If a refresh returns the same rejected AT, it remains excluded for inspection.

Disabling the switch stops new expiry marking; existing rejected ATs remain
excluded. Reauthorize with a different AT to recover them. Before rolling back to
older code, reauthorize or explicitly pause marked accounts, since older versions
do not enforce the fingerprint gate. No dependency or database migration is added.

Validation: `TestOpenAIOAuthExpiry*` covers the transport-to-existing-refresher
sequence, lifecycle wakeup, concurrent duplicates, late responses, Redis/network/
permanent failures, repeated rotations and unrelated pauses.
`TestOpenAIOAuthExpiryDatabase` verifies actual PostgreSQL field updates,
credential preservation, conditional cleanup and transactional scheduler events.
