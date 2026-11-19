package service

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/bpsbridge"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const quotaDimensionBPS = "bps"
const bpsModelPrefix = "bps-"

var bpsModelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
var bpsUnavailable = infraerrors.New(http.StatusServiceUnavailable, "BPS_UNAVAILABLE", "BPS bridge is not configured or unavailable")
var bpsNotFound = infraerrors.NotFound("BPS_SHADOW_NOT_FOUND", "BPS shadow not found")

func (a *Account) IsBPSShadow() bool {
	return a.IsShadow() && a.QuotaDimension == quotaDimensionBPS
}

func bpsParentIdentity(a *Account) (bpsbridge.Identity, bool) {
	if a == nil || a.IsShadow() || !a.IsOpenAIOAuth() || !a.IsActive() {
		return bpsbridge.Identity{}, false
	}
	id := bpsbridge.Identity{Account: a.GetChatGPTAccountID(), User: a.GetChatGPTUserID()}
	return id, id.Account != "" && id.User != ""
}

type BPSShadowOptions struct {
	Model   string `json:"model"`
	GroupID int64  `json:"group_id"`
}

func (s *adminServiceImpl) CreateBPSShadow(ctx context.Context, parentID int64, opts BPSShadowOptions) (*Account, error) {
	if _, err := bpsProcess(0); err != nil {
		return nil, err
	}
	parent, err := s.accountRepo.GetByID(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if _, ok := bpsParentIdentity(parent); !ok {
		return nil, infraerrors.BadRequest("BPS_PARENT_IDENTITY_REQUIRED", "an active OpenAI OAuth parent with a verified account and user identity is required")
	}
	model := strings.TrimSpace(opts.Model)
	if !bpsModelPattern.MatchString(model) || strings.HasPrefix(model, bpsModelPrefix) || IsGPTImageGenerationModel(model) {
		return nil, infraerrors.BadRequest("BPS_MODEL_INVALID", "specify the exact upstream BPS text model")
	}
	group, err := s.groupRepo.GetByID(ctx, opts.GroupID)
	if err != nil {
		return nil, err
	}
	if group.Platform != PlatformOpenAI {
		return nil, infraerrors.BadRequest("BPS_GROUP_INVALID", "BPS requires an OpenAI group")
	}
	if err := s.ValidateAccountGroupBindings(ctx, []int64{group.ID}); err != nil {
		return nil, err
	}
	exists := func() bool {
		children, listErr := s.accountRepo.ListShadowsByParent(ctx, parent.ID)
		return listErr == nil && findShadowDimension(children, quotaDimensionBPS) != nil
	}
	duplicate := infraerrors.New(http.StatusConflict, "BPS_SHADOW_EXISTS", "this parent already has a BPS shadow")
	if exists() {
		return nil, duplicate
	}
	name := []rune(parent.Name)
	if len(name) > 94 {
		name = name[:94]
	}
	child := &Account{
		Name: string(name) + " · BPS", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		ParentAccountID: &parent.ID, QuotaDimension: quotaDimensionBPS, Concurrency: 1, Priority: parent.Priority,
		Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{bpsModelPrefix + model: model}},
	}
	if err := s.accountRepo.Create(ctx, child); err != nil {
		if exists() {
			return nil, duplicate
		}
		return nil, err
	}
	if err := s.accountRepo.BindGroups(ctx, child.ID, []int64{group.ID}); err != nil {
		return nil, errors.Join(err, s.accountRepo.Delete(context.WithoutCancel(ctx), child.ID))
	}
	child.GroupIDs = []int64{group.ID}
	return child, nil
}

func bpsProcess(id int64) (bpsbridge.Process, error) {
	binary, root := os.Getenv("SUB2API_BPS_BINARY"), os.Getenv("SUB2API_BPS_STATE_DIR")
	if !filepath.IsAbs(binary) || !filepath.IsAbs(root) {
		return bpsbridge.Process{}, bpsUnavailable
	}
	//nolint:gosec // Absolute binary path comes from the operator's process environment.
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return bpsbridge.Process{}, bpsUnavailable
	}
	return bpsbridge.Process{Binary: binary, Dir: filepath.Join(root, fmt.Sprint(id))}, nil
}

func bpsGrantPath(p bpsbridge.Process) string { return filepath.Join(p.Dir, "grant.json") }

func bpsReadBound(id int64) (bpsbridge.BoundGrant, bpsbridge.Process, error) {
	var bound bpsbridge.BoundGrant
	p, err := bpsProcess(id)
	if err != nil {
		return bound, p, err
	}
	raw, err := bpsbridge.ReadGrantFile(bpsGrantPath(p))
	if err != nil || json.Unmarshal(raw, &bound) != nil || bound.AccessToken == "" || !time.Now().Before(bound.ExpiresAt) {
		return bpsbridge.BoundGrant{}, p, bpsbridge.ErrInvalidGrant
	}
	return bound, p, nil
}

func bpsMatchesParent(bound bpsbridge.BoundGrant, parent *Account) bool {
	id, ok := bpsParentIdentity(parent)
	return ok && bound.Identity == id
}

// BPSAuthorization is the only authorization data crossing the admin API.
type BPSAuthorization struct {
	State     string            `json:"state"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
	Session   string            `json:"session,omitempty"`
	Prompt    *bpsbridge.Prompt `json:"prompt,omitempty"`
}

type bpsAuthSession struct {
	actor int64
	id    string
	stop  context.CancelFunc
	view  BPSAuthorization
}

var bpsAuth = struct {
	sync.Mutex
	sessions map[int64]*bpsAuthSession
}{sessions: make(map[int64]*bpsAuthSession)}

func (s *adminServiceImpl) bpsTarget(ctx context.Context, id int64) (*Account, *Account, error) {
	child, err := s.accountRepo.GetByID(ctx, id)
	if err != nil || !child.IsBPSShadow() {
		return nil, nil, bpsNotFound
	}
	parent, err := s.accountRepo.GetByID(ctx, *child.ParentAccountID)
	if err != nil {
		return nil, nil, bpsNotFound
	}
	if _, ok := bpsParentIdentity(parent); !ok {
		return nil, nil, infraerrors.BadRequest("BPS_PARENT_IDENTITY_REQUIRED", "parent has no usable account and user identity")
	}
	return child, parent, nil
}

func (s *adminServiceImpl) BPSAuthorizationStatus(ctx context.Context, id, actor int64, session string) (BPSAuthorization, error) {
	_, parent, err := s.bpsTarget(ctx, id)
	if err != nil {
		return BPSAuthorization{}, err
	}
	if session != "" {
		bpsAuth.Lock()
		defer bpsAuth.Unlock()
		op := bpsAuth.sessions[id]
		if op == nil || op.actor != actor || subtle.ConstantTimeCompare([]byte(op.id), []byte(session)) != 1 {
			return BPSAuthorization{}, bpsNotFound
		}
		return op.view, nil
	}
	bound, _, err := bpsReadBound(id)
	if err == nil && bpsMatchesParent(bound, parent) {
		return BPSAuthorization{State: "authorized", ExpiresAt: &bound.ExpiresAt}, nil
	}
	return BPSAuthorization{State: "authorization_required"}, nil
}

func bpsAdminProxy(ctx context.Context, s *adminServiceImpl, p *bpsbridge.Process, parent *Account) error {
	if parent.ProxyID == nil {
		return nil
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *parent.ProxyID)
	if err != nil || proxy == nil {
		return bpsUnavailable
	}
	p.Proxy = proxy.URL()
	return nil
}

func (s *adminServiceImpl) StartBPSAuthorization(ctx context.Context, id, actor int64, method string, data []byte) (BPSAuthorization, error) {
	_, parent, err := s.bpsTarget(ctx, id)
	if err != nil {
		return BPSAuthorization{}, err
	}
	if method != "device" && (method != "paste" || len(data) == 0 || len(data) > bpsbridge.MaxGrantBytes) {
		return BPSAuthorization{}, infraerrors.BadRequest("BPS_AUTH_INPUT_INVALID", "choose device or supply a BPS authorization JSON")
	}
	p, err := bpsProcess(id)
	if err != nil || bpsAdminProxy(ctx, s, &p, parent) != nil || os.MkdirAll(p.Dir, 0700) != nil {
		return BPSAuthorization{}, bpsUnavailable
	}
	if info, statErr := os.Lstat(p.Dir); statErr != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return BPSAuthorization{}, bpsUnavailable
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return BPSAuthorization{}, bpsUnavailable
	}
	ttl := time.Minute
	if method == "device" {
		ttl = 16 * time.Minute
	}
	work, stop := context.WithTimeout(context.Background(), ttl)
	deadline, _ := work.Deadline()
	token := hex.EncodeToString(nonce[:])
	initial := BPSAuthorization{State: "pending", Session: token, ExpiresAt: &deadline}
	op := &bpsAuthSession{actor: actor, id: token, stop: stop, view: initial}
	bpsAuth.Lock()
	if old := bpsAuth.sessions[id]; old != nil && old.view.State == "pending" {
		bpsAuth.Unlock()
		stop()
		return BPSAuthorization{}, infraerrors.New(http.StatusConflict, "BPS_AUTH_PENDING", "an authorization is already pending for this shadow")
	}
	bpsAuth.sessions[id] = op
	bpsAuth.Unlock()
	time.AfterFunc(ttl+2*time.Minute, func() {
		bpsAuth.Lock()
		defer bpsAuth.Unlock()
		if bpsAuth.sessions[id] == op {
			delete(bpsAuth.sessions, id)
		}
	})
	go s.completeBPSAuthorization(work, id, op, p, method, data)
	return initial, nil
}

func (s *adminServiceImpl) completeBPSAuthorization(ctx context.Context, id int64, op *bpsAuthSession, p bpsbridge.Process, method string, raw []byte) {
	defer op.stop()
	update := func(state string, expiry *time.Time, prompt *bpsbridge.Prompt) {
		bpsAuth.Lock()
		defer bpsAuth.Unlock()
		if bpsAuth.sessions[id] == op && op.view.State == "pending" {
			op.view = BPSAuthorization{State: state, Session: op.id, ExpiresAt: expiry, Prompt: prompt}
		}
	}
	temp, err := os.MkdirTemp(p.Dir, ".pending-")
	if err != nil {
		update("unavailable", nil, nil)
		return
	}
	defer func() { _ = os.RemoveAll(temp) }()
	if method == "device" {
		device := p
		device.Dir = temp
		raw, err = device.Device(ctx, func(prompt bpsbridge.Prompt) {
			expires, _ := ctx.Deadline()
			update("pending", &expires, &prompt)
		})
		if err != nil {
			state := "device_failed"
			if ctx.Err() != nil {
				state = "expired"
			}
			update(state, nil, nil)
			return
		}
	}
	grant, identity, expiry, err := bpsbridge.VerifyGrant(ctx, raw)
	if err != nil {
		update("invalid_grant", nil, nil)
		return
	}
	bound := bpsbridge.BoundGrant{Grant: grant, Identity: identity, ExpiresAt: expiry}
	child, parent, err := s.bpsTarget(ctx, id)
	if err != nil || !bpsMatchesParent(bound, parent) {
		update("identity_mismatch", nil, nil)
		return
	}
	if err := bpsProbe(ctx, p, temp, bound, child); err != nil {
		update("probe_failed", nil, nil)
		return
	}
	bpsAuth.Lock()
	defer bpsAuth.Unlock()
	if bpsAuth.sessions[id] != op || ctx.Err() != nil {
		return
	}
	_, parent, err = s.bpsTarget(ctx, id)
	switch {
	case err != nil || !bpsMatchesParent(bound, parent):
		op.view = BPSAuthorization{State: "identity_mismatch", Session: op.id}
	case bpsbridge.Save(bpsGrantPath(p), bound) != nil:
		op.view = BPSAuthorization{State: "unavailable", Session: op.id}
	default:
		op.view = BPSAuthorization{State: "authorized", Session: op.id, ExpiresAt: &expiry}
	}
}

func (s *adminServiceImpl) CancelBPSAuthorization(_ context.Context, id, actor int64, session string) error {
	bpsAuth.Lock()
	defer bpsAuth.Unlock()
	op := bpsAuth.sessions[id]
	if op == nil || op.actor != actor || subtle.ConstantTimeCompare([]byte(op.id), []byte(session)) != 1 {
		return bpsNotFound
	}
	op.stop()
	delete(bpsAuth.sessions, id)
	return nil
}

func removeBPSAuthorization(id int64) {
	if id <= 0 {
		return
	}
	bpsAuth.Lock()
	defer bpsAuth.Unlock()
	if op := bpsAuth.sessions[id]; op != nil {
		op.stop()
		delete(bpsAuth.sessions, id)
	}
	root := os.Getenv("SUB2API_BPS_STATE_DIR")
	if filepath.IsAbs(root) {
		//nolint:gosec // The root is operator-configured and the only dynamic component is a positive integer.
		_ = os.Remove(filepath.Join(root, fmt.Sprint(id), "grant.json"))
	}
}
