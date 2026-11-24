CREATE TABLE IF NOT EXISTS fork_openai_auth_connections (
    account_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('siwc', 'excel')),
    host_id text NOT NULL,
    encrypted_grant text NOT NULL DEFAULT '',
    revision bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, kind)
);
