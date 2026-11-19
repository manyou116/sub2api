package bpsbridge

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	ClientID      = "app_fnr0pYvVwwFDocDumLG3H2Bp"
	issuer        = "https://auth.openai.com"
	MaxGrantBytes = 32 << 10
)

var ErrInvalidGrant = errors.New("invalid or expired BPS authorization")

type Grant struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type"`
	ClientID     string    `json:"client_id"`
	ExpiresIn    int64     `json:"expires_in"`
	ObtainedAt   time.Time `json:"obtained_at"`
}

type Identity struct {
	Account string `json:"chatgpt_account_id"`
	User    string `json:"chatgpt_user_id"`
}

type claims struct {
	jwt.RegisteredClaims
	ClientID string   `json:"client_id"`
	Identity Identity `json:"https://api.openai.com/auth"`
}

// VerifyGrant trusts only the signed AT claims. Exported timestamps and the
// optional ID token do not determine the account or extend the grant.
func VerifyGrant(ctx context.Context, data []byte) (Grant, Identity, time.Time, error) {
	return verifyGrant(ctx, data, issuer+"/.well-known/jwks.json")
}

func verifyGrant(ctx context.Context, data []byte, jwksURL string) (Grant, Identity, time.Time, error) {
	bad := func() (Grant, Identity, time.Time, error) { return Grant{}, Identity{}, time.Time{}, ErrInvalidGrant }
	var grant Grant
	if len(data) > MaxGrantBytes || json.Unmarshal(data, &grant) != nil || grant.AccessToken == "" ||
		!strings.EqualFold(grant.TokenType, "Bearer") || grant.ClientID != ClientID {
		return bad()
	}
	c := &claims{}
	t, err := jwt.ParseWithClaims(grant.AccessToken, c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return signingKey(ctx, jwksURL, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(issuer), jwt.WithAudience("https://api.openai.com/v1"), jwt.WithExpirationRequired())
	if err != nil || t == nil || !t.Valid || c.ClientID != ClientID || c.Identity.Account == "" || c.Identity.User == "" {
		return bad()
	}
	grant.ObtainedAt = time.Now().UTC()
	grant.ExpiresIn = int64(time.Until(c.ExpiresAt.Time).Seconds())
	return grant, c.Identity, c.ExpiresAt.Time, nil
}

func signingKey(ctx context.Context, endpoint, kid string) (*rsa.PublicKey, error) {
	if kid == "" || len(kid) > 128 {
		return nil, ErrInvalidGrant
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrInvalidGrant
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrInvalidGrant
	}
	defer func() { _ = resp.Body.Close() }()
	var set struct {
		Keys []struct{ Kid, Kty, Alg, N, E string }
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&set) != nil {
		return nil, ErrInvalidGrant
	}
	for _, k := range set.Keys {
		if k.Kid != kid || k.Kty != "RSA" || k.Alg != "RS256" {
			continue
		}
		n, nerr := base64.RawURLEncoding.DecodeString(k.N)
		e, eerr := base64.RawURLEncoding.DecodeString(k.E)
		if nerr != nil || eerr != nil || len(n) < 256 || len(e) == 0 || len(e) > 4 {
			return nil, ErrInvalidGrant
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		if exponent < 3 || exponent > 1<<31-1 || exponent%2 == 0 {
			return nil, ErrInvalidGrant
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}, nil
	}
	return nil, ErrInvalidGrant
}
