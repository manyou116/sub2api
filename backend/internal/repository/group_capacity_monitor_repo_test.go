//go:build unit

package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestCapacityMonitorSnapshotError(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, capacityMonitorSnapshotError(ctx, "history", nil))
	for _, cause := range []error{context.DeadlineExceeded, &pq.Error{Code: "57014", Message: "canceling statement due to statement timeout"}} {
		err := capacityMonitorSnapshotError(ctx, "history", fmt.Errorf("query: %w", cause))
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorIs(t, err, cause)
		require.Contains(t, err.Error(), "capacity monitor history")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err := capacityMonitorSnapshotError(canceled, "member identities", &pq.Error{Code: "57014"})
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
	cause := errors.New("database unavailable")
	err = capacityMonitorSnapshotError(ctx, "begin snapshot", cause)
	require.ErrorIs(t, err, cause)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
}
