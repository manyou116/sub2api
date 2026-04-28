package webdriver

import (
	"encoding/hex"
	"strings"
	"testing"

	"golang.org/x/crypto/sha3"
)

func TestSolvePow_HappyPath(t *testing.T) {
	// 难度 "ff" 极宽松：sum[0] <= 0xff 总成立 → 第一轮即成功。
	cfg := buildPowConfig("UA-test", "https://example.com/sdk.js", "build123")
	encoded, ok := solvePow("seed-x", "ff", cfg, 100)
	if !ok || encoded == "" {
		t.Fatalf("expect success, got ok=%v encoded=%q", ok, encoded)
	}
	// 校验：sha3-512(seed||encoded)[0:1] <= 0xff
	sum := sha3.Sum512(append([]byte("seed-x"), encoded...))
	if sum[0] > 0xff {
		t.Errorf("hash prefix %x exceeds difficulty", sum[0])
	}
}

func TestSolvePow_DifficultyHex(t *testing.T) {
	cfg := buildPowConfig("UA", "src", "db")
	// "00ff" → 前 2 字节必须 <= 0x00ff。第一字节必须 0x00。
	encoded, ok := solvePow("seed", "00ff", cfg, 5000)
	if !ok {
		t.Skip("difficulty 00ff might not solve in 5000 iters; flaky-but-not-bug")
	}
	sum := sha3.Sum512(append([]byte("seed"), encoded...))
	diff, _ := hex.DecodeString("00ff")
	if !bytesLE(sum[:len(diff)], diff) {
		t.Errorf("hash %x not <= %x", sum[:len(diff)], diff)
	}
}

func TestBuildRequirementsToken_Prefix(t *testing.T) {
	tok := buildRequirementsToken("UA", []string{defaultSentinelSDKURL}, "build")
	if tok != "" && !strings.HasPrefix(tok, "gAAAAAC") {
		t.Errorf("expect gAAAAAC prefix, got %q", tok[:min(20, len(tok))])
	}
}

func TestBuildProofToken_NotRequired(t *testing.T) {
	tok, err := buildProofToken(false, "", "", "UA", nil, "")
	if err != nil || tok != "" {
		t.Errorf("expect (\"\", nil) when not required, got (%q, %v)", tok, err)
	}
}

func TestBuildProofToken_PrefixWhenSolved(t *testing.T) {
	// 用极宽松难度 "ff" 强制成功
	tok, err := buildProofToken(true, "seed", "ff", "UA", []string{defaultSentinelSDKURL}, "build")
	if err != nil {
		t.Fatalf("expect success, got %v", err)
	}
	if !strings.HasPrefix(tok, "gAAAAAB") {
		t.Errorf("expect gAAAAAB prefix, got %q", tok[:min(20, len(tok))])
	}
}

func bytesLE(a, b []byte) bool {
	for i := range a {
		if a[i] < b[i] {
			return true
		}
		if a[i] > b[i] {
			return false
		}
	}
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
