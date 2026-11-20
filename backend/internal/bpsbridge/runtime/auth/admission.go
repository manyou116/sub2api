package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

// AdmissionKey identifies one user within a ChatGPT account for local admission
// and pacing. Different users may share an account/workspace ID. Tokens and
// grant filenames are deliberately excluded so renewal and duplicate grants
// cannot reset the user's rolling window.
//
// An absent subject retains the legacy account key for standalone credentials.
// This key is internal routing state, never an upstream account header.
func AdmissionKey(accountID, subject string) string {
	if accountID == "" || subject == "" {
		return accountID
	}
	sum := sha256.Sum256([]byte(subject + "\x00" + accountID))
	return "bps-user-v1:" + hex.EncodeToString(sum[:])
}

// AdmissionKeyFromToken derives local routing identity from the supplied grant.
// Decoding claims is not token verification; the upstream still authenticates it.
func AdmissionKeyFromToken(token string) string {
	claims := decodeJWT(token)
	subject, _ := claims["sub"].(string)
	return AdmissionKey(AccountIDFromToken(token), subject)
}
