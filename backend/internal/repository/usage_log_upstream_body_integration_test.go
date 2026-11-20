//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func (s *UsageLogRepoSuite) TestUpstreamRequestBodyRoundTrip() {
	user := mustCreateUser(s.T(), s.client, &service.User{Email: "body-stats@test.com"})
	key := mustCreateApiKey(s.T(), s.client, &service.APIKey{UserID: user.ID, Key: "sk-body-stats", Name: "k"})
	account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "body-stats"})
	for _, stats := range []*service.UpstreamRequestBodyStats{nil, {
		RawBytes: 2048, EncodedBytes: 1280, Attempts: 2, CompressedAttempts: 1, Fallbacks: 1,
		Encoding: "identity", Reason: "encoding_rejected", FinalRawBytes: 1024, FinalEncodedBytes: 1024,
	}} {
		log := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, RequestID: uuid.NewString(), Model: "gpt-test", UpstreamRequestBody: stats}
		inserted, err := s.repo.Create(s.ctx, log)
		s.Require().NoError(err)
		s.Require().True(inserted)
		got, err := s.repo.GetByID(s.ctx, log.ID)
		s.Require().NoError(err)
		s.Require().Equal(stats, got.UpstreamRequestBody)
	}
}

func TestUsageLogUpstreamBodyBatchRoundTrip(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := newUsageLogRepositoryWithSQL(client, integrationDB)
	user := mustCreateUser(t, client, &service.User{Email: uuid.NewString() + "@body.test"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: uuid.NewString(), Name: "k"})
	account := mustCreateAccount(t, client, &service.Account{Name: "body-batch"})
	stats := &service.UpstreamRequestBodyStats{RawBytes: 1024, EncodedBytes: 256, Attempts: 1, CompressedAttempts: 1, Encoding: "zstd", Reason: "compressed", FinalRawBytes: 1024, FinalEncodedBytes: 256}
	for _, bestEffort := range []bool{false, true} {
		log := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID, RequestID: uuid.NewString(), Model: "gpt-test", UpstreamRequestBody: stats}
		if bestEffort {
			require.NoError(t, repo.CreateBestEffort(ctx, log))
		} else {
			inserted, err := repo.Create(ctx, log)
			require.NoError(t, err)
			require.True(t, inserted)
		}
		var id int64
		require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT id FROM usage_logs WHERE api_key_id = $1 AND request_id = $2", key.ID, log.RequestID).Scan(&id))
		got, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		require.Equal(t, stats, got.UpstreamRequestBody)
	}
}
