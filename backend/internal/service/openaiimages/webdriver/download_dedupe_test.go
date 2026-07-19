package webdriver

import "testing"

func TestMergeDownloadCandidatesDedupe(t *testing.T) {
	id := "file_00000000aaaaaaaaaaaaaaaaaaaaaaaa"
	got := mergeDownloadCandidates(
		[]string{id, "file-service://" + id},
		[]string{"sediment://" + id, id},
	)
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d %#v", len(got), got)
	}
	if got[0].ID != id {
		t.Fatalf("id=%s", got[0].ID)
	}
	if got[0].Sediment {
		t.Fatalf("expected file route preferred for file_ ids")
	}
}
