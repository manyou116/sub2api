package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func admissionToken(account, subject, nonce string) string {
	payload, _ := json.Marshal(map[string]any{"sub": subject, "nonce": nonce,
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account}})
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestAdmissionKeyPreservesUserAcrossRenewalsAndSeparatesWorkspaceMembers(t *testing.T) {
	want := "bps-user-v1:ddd7f88f2edb4b0fb45acba05db96889f9a453c9e5468f7c84782aa4a58d7fac"
	if got := AdmissionKey("workspace", "user-a"); got != want {
		t.Fatalf("unexpected admission key: %s", got)
	}
	for _, nonce := range []string{"old-access-token", "renewed-access-token"} {
		if got := AdmissionKeyFromToken(admissionToken("workspace", "user-a", nonce)); got != want {
			t.Fatal("token renewal changed user identity")
		}
	}
	if AdmissionKey("workspace", "user-b") == want || AdmissionKey("another-workspace", "user-a") == want {
		t.Fatal("different user or workspace shared the admission key")
	}
	for _, tc := range []struct{ account, subject, want string }{{"workspace", "", "workspace"}, {"", "user-a", ""}, {"", "", ""}} {
		if AdmissionKey(tc.account, tc.subject) != tc.want || AdmissionKeyFromToken(admissionToken(tc.account, tc.subject, "")) != tc.want {
			t.Fatalf("legacy fallback failed: %#v", tc)
		}
	}
	if AdmissionKeyFromToken("malformed") != "" {
		t.Fatal("malformed token supplied an admission identity")
	}
}
