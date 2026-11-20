package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSubmitUsageRecordTaskCopiesRequestContext(t *testing.T) {
	parent := context.WithValue(context.Background(), ctxkey.ClientRequestID, "client-request-123")
	parent = context.WithValue(parent, ctxkey.RequestID, "request-456")

	var gotClientRequestID string
	var gotRequestID string
	h := &GatewayHandler{}
	h.submitUsageRecordTask(parent, func(ctx context.Context) {
		gotClientRequestID, _ = ctx.Value(ctxkey.ClientRequestID).(string)
		gotRequestID, _ = ctx.Value(ctxkey.RequestID).(string)
	})

	require.Equal(t, "client-request-123", gotClientRequestID)
	require.Equal(t, "request-456", gotRequestID)
}

func TestOpenAISubmitUsageRecordTaskCopiesRequestContext(t *testing.T) {
	parent := context.WithValue(context.Background(), ctxkey.ClientRequestID, "openai-client-request-123")
	parent = context.WithValue(parent, ctxkey.RequestID, "openai-request-456")

	var gotClientRequestID string
	var gotRequestID string
	h := &OpenAIGatewayHandler{}
	h.submitUsageRecordTask(parent, func(ctx context.Context) {
		gotClientRequestID, _ = ctx.Value(ctxkey.ClientRequestID).(string)
		gotRequestID, _ = ctx.Value(ctxkey.RequestID).(string)
	})

	require.Equal(t, "openai-client-request-123", gotClientRequestID)
	require.Equal(t, "openai-request-456", gotRequestID)
}

func TestSourceUserSnapshotSurvivesAsyncUsageTask(t *testing.T) {
	identity := &service.SourceUser{SourceID: "factory", UserID: "123", Username: "Alice"}
	parent := service.WithSourceUser(context.Background(), identity)
	identity.UserID = "forged-after-snapshot"
	handler := &GatewayHandler{usageRecordWorkerPool: newUsageRecordTestPool(t)}
	done := make(chan *service.SourceUser, 1)
	handler.submitUsageRecordTask(parent, func(ctx context.Context) { done <- service.SourceUserFromContext(ctx) })
	got := <-done
	require.Equal(t, &service.SourceUser{SourceID: "factory", UserID: "123", Username: "Alice"}, got)
}

func TestUsageRecordContextPreservesSourceAfterRequestCancellation(t *testing.T) {
	identity := &service.SourceUser{SourceID: "factory", UserID: "123", Username: "Alice"}
	parent, cancel := context.WithCancel(service.WithSourceUser(context.Background(), identity))
	t.Cleanup(cancel)
	recordContext := usageRecordContext(parent, context.Background())
	cancel()
	identity.Username = "renamed-after-snapshot"
	require.NoError(t, recordContext.Err())
	require.Equal(t, "Alice", service.SourceUserFromContext(recordContext).Username)
}
