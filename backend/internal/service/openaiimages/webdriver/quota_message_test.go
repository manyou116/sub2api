package webdriver

import "testing"

func TestIsImageQuotaLimitedMessage(t *testing.T) {
	if !IsImageQuotaLimitedMessage(`You've hit the Free plan limit for image generations requests. resets in 22 hours`) {
		t.Fatal("expected image quota")
	}
	if IsImageQuotaLimitedMessage("soft poll 429") {
		t.Fatal("soft poll is not quota")
	}
	if IsImageQuotaLimitedMessage("conversation poll rate limited (429 x8)") {
		t.Fatal("poll 429 is not quota")
	}
	if IsImageQuotaLimitedMessage("Too Many Requests") {
		t.Fatal("bare 429 body is not quota")
	}
}
