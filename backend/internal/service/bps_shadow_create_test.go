//go:build unit

package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBPSCreateInheritsParentGroupsWithoutModelOrBridge(t *testing.T) {
	t.Setenv("SUB2API_BPS_STATE_DIR", "relative-path")
	repo := newSparkShadowRepoStub()
	admin := &adminServiceImpl{accountRepo: repo}
	parent := &Account{Name: "parent", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		GroupIDs:    []int64{11},
		Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
	require.NoError(t, repo.Create(t.Context(), parent))
	child, err := admin.CreateBPSShadow(t.Context(), parent.ID, BPSShadowOptions{})
	require.NoError(t, err)
	require.True(t, child.IsBPSShadow())
	require.Equal(t, []int64{11}, child.GroupIDs)
	require.Equal(t, []int64{11}, repo.groupsOf[child.ID])
	require.Empty(t, child.Credentials)
	require.False(t, parentHealthyForShadow(child, func(int64) *Account { return parent }))
	status, err := admin.BPSAuthorizationStatus(t.Context(), child.ID, 1, "")
	require.NoError(t, err)
	require.Equal(t, "authorization_required", status.State)
	_, err = admin.StartBPSAuthorization(t.Context(), child.ID, 1, "device", nil)
	require.ErrorIs(t, err, bpsUnavailable)
}

func TestBPSCreateFallsBackToOpenAIDefaultGroup(t *testing.T) {
	repo := newSparkShadowRepoStub()
	admin := &adminServiceImpl{accountRepo: repo, groupRepo: &sparkShadowGroupRepoStub{
		groups: []Group{{ID: 99, Name: PlatformOpenAI + "-default"}},
	}}
	parent := &Account{Name: "parent", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
	require.NoError(t, repo.Create(t.Context(), parent))
	child, err := admin.CreateBPSShadow(t.Context(), parent.ID, BPSShadowOptions{})
	require.NoError(t, err)
	require.Equal(t, []int64{99}, child.GroupIDs)
	require.Equal(t, []int64{99}, repo.groupsOf[child.ID])
}

func TestBPSCreateAllowsAuthorizationWithoutAnyGroup(t *testing.T) {
	t.Setenv("SUB2API_BPS_STATE_DIR", "")
	t.Setenv("DATA_DIR", t.TempDir())
	repo := newSparkShadowRepoStub()
	admin := &adminServiceImpl{accountRepo: repo, groupRepo: &sparkShadowGroupRepoStub{}}
	parent := &Account{Name: "parent", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
	require.NoError(t, repo.Create(t.Context(), parent))
	child, err := admin.CreateBPSShadow(t.Context(), parent.ID, BPSShadowOptions{})
	require.NoError(t, err)
	require.Empty(t, child.GroupIDs)
	require.NotContains(t, repo.groupsOf, child.ID)
	status, err := admin.BPSAuthorizationStatus(t.Context(), child.ID, 1, "")
	require.NoError(t, err)
	require.Equal(t, "authorization_required", status.State)
}

func TestBPSRuntimeStatePath(t *testing.T) {
	data := t.TempDir()
	t.Setenv("DATA_DIR", data)
	t.Setenv("SUB2API_BPS_STATE_DIR", "")
	r, err := bpsRuntime(42)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(data, "bps", "42"), r.Dir)
	explicit := filepath.Join(data, "custom")
	t.Setenv("SUB2API_BPS_STATE_DIR", explicit)
	r, err = bpsRuntime(42)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(explicit, "42"), r.Dir)
	require.NoError(t, os.Mkdir(explicit, 0700))
	require.NoError(t, os.Symlink(data, r.Dir))
	_, err = bpsRuntime(42)
	require.ErrorIs(t, err, bpsUnavailable)
}
