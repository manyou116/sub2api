//go:build integration

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	rediscontainer "github.com/testcontainers/testcontainers-go/modules/redis"
)

func TestWebImageRealRedisConcurrentAdmission(t *testing.T) {
	ctx := context.Background()
	container, err := rediscontainer.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, container.Terminate(ctx)) })
	endpoint, err := container.Endpoint(ctx, "")
	require.NoError(t, err)
	testWebImageRedisConcurrentAdmission(t, endpoint)
}
