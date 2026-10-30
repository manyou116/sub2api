//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Exercise the real filename/checksum runner from both previously shipped lines.
// Each subtest owns a fresh database inside the disposable integration container.
func TestImageShadowPublishedUpgradePaths(t *testing.T) {
	ctx := context.Background()
	for _, line := range []string{"plus13", "v0219_without_shadows"} {
		t.Run(line, func(t *testing.T) {
			name := fmt.Sprintf("shadow_upgrade_%d", time.Now().UnixNano())
			_, err := integrationDB.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(name))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, dropErr := integrationDB.ExecContext(ctx, "DROP DATABASE "+pq.QuoteIdentifier(name)+" WITH (FORCE)")
				require.NoError(t, dropErr)
			})
			dsn, err := url.Parse(integrationDSN)
			require.NoError(t, err)
			dsn.Path = "/" + name
			db, err := sql.Open("postgres", dsn.String())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			baseline := fstest.MapFS{}
			names, err := fs.Glob(migrations.FS, "*.sql")
			require.NoError(t, err)
			for _, filename := range names {
				shadow := filename == "246_web_image_shadows.sql" || filename == "247_work_image_shadows.sql"
				if (line == "plus13" && filename >= "246_" && !shadow) || (line != "plus13" && shadow) {
					continue
				}
				body, readErr := migrations.FS.ReadFile(filename)
				require.NoError(t, readErr)
				baseline[filename] = &fstest.MapFile{Data: body}
			}
			require.NoError(t, applyMigrationsFS(ctx, db, baseline))
			var parentID int64
			require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO accounts
				(name, platform, type, credentials, concurrency, web_image_rate_limit_reset_at)
				VALUES ('upgrade-parent', 'openai', 'oauth', '{"access_token":"fixture"}', 3, NOW() + INTERVAL '1 hour') RETURNING id`).Scan(&parentID))
			if line == "plus13" {
				for _, purpose := range []string{"spark", "web_images", "work_images"} {
					_, err = db.ExecContext(ctx, `INSERT INTO accounts
						(name, platform, type, credentials, parent_account_id, quota_dimension, concurrency)
						VALUES ($1, 'openai', 'oauth', '{}', $2, $1, 2)`, purpose, parentID)
					require.NoError(t, err)
				}
			}
			for range 2 {
				require.NoError(t, ApplyMigrations(ctx, db), "upgrade and restart must preserve shipped checksums")
			}
			if line != "plus13" {
				for _, purpose := range []string{"web_images", "work_images"} {
					_, err = db.ExecContext(ctx, `INSERT INTO accounts
						(name, platform, type, credentials, parent_account_id, quota_dimension, concurrency)
						VALUES ($1, 'openai', 'oauth', '{}', $2, $1, 2)`, purpose, parentID)
					require.NoError(t, err)
				}
			}
			var purposes string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT string_agg(quota_dimension, ',' ORDER BY quota_dimension)
				FROM accounts WHERE parent_account_id = $1`, parentID).Scan(&purposes))
			require.Contains(t, purposes, "web_images")
			require.Contains(t, purposes, "work_images")
			if line == "plus13" {
				require.Equal(t, "spark,web_images,work_images", purposes)
			}
			var cooldownRetained bool
			require.NoError(t, db.QueryRowContext(ctx, `SELECT web_image_rate_limit_reset_at > NOW()
				FROM accounts WHERE id = $1`, parentID).Scan(&cooldownRetained))
			require.True(t, cooldownRetained)
			var applied int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations
				WHERE filename IN ('246_web_image_shadows.sql', '247_work_image_shadows.sql',
				'246_enable_gemini_image_generation_groups.sql', '247_scoped_admin_api_keys.sql')`).Scan(&applied))
			require.Equal(t, 4, applied, "same numeric prefixes are independent full filenames")
		})
	}
}
