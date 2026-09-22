package repository

import (
	"context"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxybudget"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// budgetingHTTPUpstream is a default-off admission decorator around the shared
// upstream port. It measures application bodies; TLS and wire framing are a
// fixed conservative allowance rather than claimed provider-metered bytes.
type budgetingHTTPUpstream struct {
	next   service.HTTPUpstream
	budget *proxybudget.Client
}

func newBudgetingHTTPUpstream(next service.HTTPUpstream) service.HTTPUpstream {
	budget := proxybudget.NewFromEnv()
	if budget.Disabled() {
		return next
	}
	return &budgetingHTTPUpstream{next: next, budget: budget}
}

func (u *budgetingHTTPUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	return u.do(req, proxyURL, func(request *http.Request) (*http.Response, error) {
		return u.next.Do(request, proxyURL, accountID, accountConcurrency)
	})
}

func (u *budgetingHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.do(req, proxyURL, func(request *http.Request) (*http.Response, error) {
		return u.next.DoWithTLS(request, proxyURL, accountID, accountConcurrency, profile)
	})
}

func (u *budgetingHTTPUpstream) do(req *http.Request, proxyURL string, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if u == nil || u.next == nil {
		return nil, io.ErrClosedPipe
	}
	ctx := context.Background()
	if req != nil && req.Context() != nil {
		ctx = req.Context()
	}
	lease, err := u.budget.OpenLease(ctx, proxyURL)
	if err != nil {
		return nil, err
	}
	if req != nil && req.Body != nil && lease.Active() {
		req.Body = proxybudget.WrapBody(req.Body, lease, false)
	}
	resp, err := send(req)
	if err != nil {
		_ = lease.Settle(context.Background())
		return nil, err
	}
	if resp != nil && resp.Body != nil && lease.Active() {
		resp.Body = proxybudget.WrapBody(resp.Body, lease, true)
	} else {
		_ = lease.Settle(context.Background())
	}
	return resp, nil
}
