package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *accountRepository) EnsureOpenAIConnection(ctx context.Context, id int64, kind, host string) error {
	_, err := r.sql.ExecContext(ctx, `
		INSERT INTO fork_openai_auth_connections (account_id, kind, host_id)
		SELECT id, $2, $3 FROM accounts WHERE id = $1 AND deleted_at IS NULL
		ON CONFLICT (account_id, kind) DO NOTHING`, id, kind, host)
	return err
}

func (r *accountRepository) ReadOpenAIConnection(ctx context.Context, id int64, kind string) (service.ForkOpenAIConnection, error) {
	var record service.ForkOpenAIConnection
	err := scanSingleRow(ctx, r.sql, `
		SELECT c.host_id, c.encrypted_grant, c.revision
		FROM fork_openai_auth_connections c JOIN accounts a ON a.id = c.account_id
		WHERE c.account_id = $1 AND c.kind = $2 AND a.deleted_at IS NULL`,
		[]any{id, kind}, &record.HostID, &record.Ciphertext, &record.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return record, nil
	}
	return record, err
}

func (r *accountRepository) SaveOpenAIConnection(ctx context.Context, id int64, kind string, revision int64, identityEmail, identityWorkspace, ciphertext string) error {
	result, err := r.sql.ExecContext(ctx, `
		UPDATE fork_openai_auth_connections c
		SET encrypted_grant = $6, revision = c.revision + 1
		FROM accounts a
		WHERE c.account_id = $1 AND c.kind = $2 AND c.revision = $3
		AND a.id = c.account_id AND a.deleted_at IS NULL AND a.platform = 'openai' AND a.type = 'oauth'
		AND lower(btrim(a.credentials->>'email')) = lower($4)
		AND coalesce(a.credentials->>'chatgpt_account_id', '') = $5`,
		id, kind, revision, identityEmail, identityWorkspace, ciphertext)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("OpenAI authorization changed or no longer matches the account")
	}
	return nil
}
