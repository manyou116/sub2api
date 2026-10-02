package proxybudget

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPythonLedgerContract uses an explicitly enabled local Python/PostgreSQL
// fixture; normal tests never call a running application or production ledger.
func TestPythonLedgerContract(t *testing.T) {
	base := os.Getenv("PROXY_BUDGET_TEST_URL")
	if base == "" {
		t.Skip("set PROXY_BUDGET_TEST_URL to the isolated Python fixture")
	}
	client := New(base, "local-test-only", nil)
	lease, err := client.OpenLease(context.Background(), "http://user:pass@proxy.invalid:8080")
	require.NoError(t, err)
	require.True(t, lease.Managed())
	require.Equal(t, ModeEnforce, lease.Mode())
	for i := 0; i < 4; i++ {
		reservation, err := lease.ReserveIO(context.Background(), 64<<10)
		require.NoError(t, err)
		reservation.Finish(50 << 10)
	}
	require.NoError(t, lease.Settle(context.Background()))
	require.NoError(t, lease.Settle(context.Background()))
}
