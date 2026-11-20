package gateway

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// The persistent state key namespaces task slots to this installation.
// Slot identity survives restarts and token rotation, but never crosses users or
// workspaces. Caller/history scopes deliberately do not participate in this ID.
func slotTaskID(key, account string, slot int) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("bps-account-slot-v1\x00" + account + "\x00" + strconv.Itoa(slot)))
	return hex.EncodeToString(mac.Sum(nil))
}

// Every dispatched generation gets a fresh turn, even for identical inputs on
// the same slot. No task rotation or automatic replay is performed on errors.
func newSlotTurnID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
