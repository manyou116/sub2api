package bpsbridge

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func TestBPSGrantVerifiesSignedProviderAndIdentity(t *testing.T) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	n := base64.RawURLEncoding.EncodeToString(private.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.E)).Bytes())
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": "bps-test", "alg": "RS256", "kty": "RSA", "n": n, "e": e}}})
	}))
	defer keys.Close()

	for _, tc := range []struct {
		name, client, audience string
		expiry                 time.Duration
		accepted               bool
	}{
		{"BPS", "" + ClientID, "https://api.openai.com/v1", time.Hour, true},
		{"Codex", "codex-client", "https://api.openai.com/v1", time.Hour, false},
		{"expired", ClientID, "https://api.openai.com/v1", -time.Hour, false},
		{"wrong audience", ClientID, "wrong-audience", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := Identity{Account: "test-workspace", User: "test-user"}
			signed := jwt.NewWithClaims(jwt.SigningMethodRS256, claims{
				RegisteredClaims: jwt.RegisteredClaims{Issuer: issuer, Audience: jwt.ClaimStrings{tc.audience}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(tc.expiry))},
				ClientID:         tc.client, Identity: id,
			})
			signed.Header["kid"] = "bps-test"
			at, err := signed.SignedString(private)
			require.NoError(t, err)
			raw, err := json.Marshal(Grant{AccessToken: at, RefreshToken: "fixture-refresh", TokenType: "Bearer", ClientID: ClientID, ExpiresIn: 10_000_000})
			require.NoError(t, err)
			normalized, identity, expires, err := verifyGrant(t.Context(), raw, keys.URL)
			if !tc.accepted {
				require.ErrorIs(t, err, ErrInvalidGrant)
				return
			}
			require.NoError(t, err)
			require.Equal(t, id, identity)
			require.LessOrEqual(t, normalized.ExpiresIn, int64(3600))
			require.WithinDuration(t, time.Now().Add(time.Hour), expires, 2*time.Second)
		})
	}
}
