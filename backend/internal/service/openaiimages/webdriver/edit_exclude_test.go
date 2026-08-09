package webdriver

import "testing"

func TestFilterAssetIDs_ExcludesInputs(t *testing.T) {
	ex := map[string]struct{}{"file_input": {}, "file_00000000aaaaaaaaaaaaaaaaaaaaaaaa": {}}
	got := filterAssetIDs([]string{"file_input", "file_output", "file-service://file_input"}, ex)
	// filterAssetIDs normalizes only for exclude check; keeps original id string if not excluded
	if len(got) != 1 || got[0] != "file_output" {
		t.Fatalf("got %#v", got)
	}
}

func TestInputAssetExcludeSet(t *testing.T) {
	refs := []map[string]any{{"file_id": "file_abc"}, {"file_id": "  file_def  "}}
	ex := inputAssetExcludeSet(refs)
	if _, ok := ex["file_abc"]; !ok {
		t.Fatal("missing abc")
	}
	if _, ok := ex["file_def"]; !ok {
		t.Fatal("missing def")
	}
}
