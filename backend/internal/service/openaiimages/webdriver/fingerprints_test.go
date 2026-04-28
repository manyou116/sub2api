package webdriver

import "testing"

func TestPickFingerprintDeterministic(t *testing.T) {
	for _, id := range []int64{1, 7, 42, 1234567} {
		a := PickFingerprint(id)
		b := PickFingerprint(id)
		if a.Name != b.Name {
			t.Fatalf("PickFingerprint(%d) not deterministic: %q != %q", id, a.Name, b.Name)
		}
	}
}

func TestPickFingerprintZeroFallback(t *testing.T) {
	if got := PickFingerprint(0).Name; got != fingerprints[0].Name {
		t.Fatalf("PickFingerprint(0) = %q, want %q", got, fingerprints[0].Name)
	}
	if got := PickFingerprint(-99).Name; got != fingerprints[0].Name {
		t.Fatalf("PickFingerprint(-99) = %q, want %q", got, fingerprints[0].Name)
	}
}

func TestPickFingerprintSpread(t *testing.T) {
	// 当前策略：所有账号统一使用 fingerprints[0]（详见 PickFingerprint doc）。
	// 池中其余 profile 保留作未来 A/B 测试与 chatgpt2api 对齐参考。
	for i := int64(1); i <= 20; i++ {
		if got := PickFingerprint(i).Name; got != fingerprints[0].Name {
			t.Fatalf("PickFingerprint(%d) = %q, want %q (uniform-profile policy)", i, got, fingerprints[0].Name)
		}
	}
}

func TestFingerprintProfilesConsistent(t *testing.T) {
	// 健全性：每个 profile 的 sec-ch-ua 主版本必须出现在 UA 字符串里。
	for _, fp := range fingerprints {
		// 从 SecChUaFullVersion 抽出主版本（第一个 . 之前）
		ver := fp.SecChUaFullVersion
		if len(ver) < 4 || ver[0] != '"' {
			t.Fatalf("%s: SecChUaFullVersion shape unexpected: %q", fp.Name, ver)
		}
		// 期待 UA 中至少出现 sec-ch-ua 中声明的主版本号
		if !contains(fp.UserAgent, fp.SecChUaFullVersion[1:4]) {
			t.Fatalf("%s: UA %q does not contain version prefix %q", fp.Name, fp.UserAgent, fp.SecChUaFullVersion[1:4])
		}
		if fp.TLSHello.Client == "" {
			t.Fatalf("%s: TLSHello empty", fp.Name)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
