package webdriver

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"strings"
	"time"

	"golang.org/x/crypto/sha3"
)

func buildRequirementsToken(userAgent string) string {
	raw, _ := json.Marshal(buildPoWConfig(userAgent))
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(raw)
}

func buildProofToken(seed, difficulty, userAgent string) (string, error) {
	answer, ok := solvePoW(seed, difficulty, buildPoWConfig(userAgent), 500000)
	if !ok {
		return "", fmt.Errorf("failed to solve proof token")
	}
	return "gAAAAAB" + answer, nil
}

func buildPoWConfig(userAgent string) []any {
	now := time.Now().UTC()
	dateStr := now.Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)")
	perf := 1000 + mrand.Float64()*49000
	cores := []int{4, 8, 12, 16}
	return []any{"1920x1080", dateStr, 4294705152, mrand.Float64(), userAgent,
		"https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js", nil, nil, "en-US", mrand.Float64(),
		"plugins-undefined", "location", "Object", perf, randomUUID(), "", cores[mrand.Intn(len(cores))],
		time.Now().UnixMilli() - int64(perf), 0, 0, 0, 0, 0, 0, 0}
}

func solvePoW(seed, difficulty string, config []any, limit int) (string, bool) {
	diff := strings.TrimSpace(difficulty)
	if diff == "" {
		diff = "0"
	}
	target, err := hex.DecodeString(diff)
	if err != nil {
		return "", false
	}
	diffLen := len(target)
	seedBytes := []byte(seed)
	prefix, _ := json.Marshal(config[:3])
	static1 := append(prefix[:len(prefix)-1], ',')
	mid, _ := json.Marshal(config[4:9])
	static2 := append(append([]byte{','}, mid[1:len(mid)-1]...), ',')
	suffix, _ := json.Marshal(config[10:])
	static3 := append([]byte{','}, suffix[1:]...)
	for i := 0; i < limit; i++ {
		final := make([]byte, 0, len(static1)+len(static2)+len(static3)+24)
		final = append(final, static1...)
		final = append(final, []byte(fmt.Sprintf("%d", i))...)
		final = append(final, static2...)
		final = append(final, []byte(fmt.Sprintf("%d", i>>1))...)
		final = append(final, static3...)
		encoded := base64.StdEncoding.EncodeToString(final)
		sum := sha3.Sum512(append(seedBytes, encoded...))
		if bytesLE(sum[:diffLen], target) {
			return encoded, true
		}
	}
	return "", false
}

func bytesLE(a, b []byte) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] < b[i] {
			return true
		}
		if a[i] > b[i] {
			return false
		}
	}
	return len(a) <= len(b)
}

func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
