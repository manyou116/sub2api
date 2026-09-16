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
		req.Body = &budgetingBody{ReadCloser: req.Body, lease: lease}
	}
	resp, err := send(req)
	if err != nil {
		_ = lease.Settle(context.Background())
		return nil, err
	}
	if resp != nil && resp.Body != nil && lease.Active() {
		resp.Body = &budgetingBody{ReadCloser: resp.Body, lease: lease, settleOnFinish: true}
	} else {
		_ = lease.Settle(context.Background())
	}
	return resp, nil
}

// budgetingBody caps each source read to a pre-admitted 64 KiB application
// chunk and settles exactly once on response EOF, failure, or close.
type budgetingBody struct {
	io.ReadCloser
	lease          *proxybudget.Lease
	settleOnFinish bool
	settled        bool
}

func (b *budgetingBody) Read(p []byte) (int, error) {
	if b == nil || b.ReadCloser == nil {
		return 0, io.ErrClosedPipe
	}
	limit, err := b.lease.BeforeRead(context.Background(), len(p))
	if err != nil {
		b.settle()
		return 0, err
	}
	n, readErr := b.ReadCloser.Read(p[:limit])
	b.lease.Observe(n)
	if readErr != nil {
		b.settle()
	}
	return n, readErr
}

func (b *budgetingBody) Close() error {
	if b == nil || b.ReadCloser == nil {
		return nil
	}
	err := b.ReadCloser.Close()
	b.settle()
	return err
}

func (b *budgetingBody) settle() {
	if b == nil || !b.settleOnFinish || b.settled {
		return
	}
	b.settled = true
	_ = b.lease.Settle(context.Background())
}
