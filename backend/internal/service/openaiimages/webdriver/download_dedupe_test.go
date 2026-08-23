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

func TestExtractDownloadAttachmentFileIDsPrefersTargetSizeFiles(t *testing.T) {
	body := []byte(`{
		"content": "preview file-service://file_00000000aaaaaaaaaaaaaaaaaaaaaaaa",
		"attachments": [
			{"file_id":"file_00000000bbbbbbbbbbbbbbbbbbbbbbbb","name":"preview.png"},
			{"file_id":"file_00000000cccccccccccccccccccccccc","name":"poster-2160x3840.png"}
		]
	}`)
	got := extractDownloadAttachmentFileIDs(body, "2160x3840")
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d %#v", len(got), got)
	}
	if got[0] != "file_00000000cccccccccccccccccccccccc" {
		t.Fatalf("candidate=%s", got[0])
	}
}

func TestExtractDownloadAttachmentFileIDsMatchesWidthHeight(t *testing.T) {
	body := []byte(`{
		"attachments": [
			{"file_id":"file_00000000dddddddddddddddddddddddd","name":"image.png","width":2160,"height":3840}
		]
	}`)
	got := extractDownloadAttachmentFileIDs(body, "2160x3840")
	if len(got) != 1 || got[0] != "file_00000000dddddddddddddddddddddddd" {
		t.Fatalf("got %#v", got)
	}
}

func TestExtractDownloadAttachmentFileIDsSupportsFileDashID(t *testing.T) {
	body := []byte(`{
		"mapping": {
			"reply": {
				"message": {
					"metadata": {
						"attachments": [
							{"id":"file-AbCdEf123456","name":"cat-2160×3840.png","mime_type":"image/png","width":2160,"height":3840}
						]
					}
				}
			}
		}
	}`)
	got := extractDownloadAttachmentFileIDs(body, "2160x3840")
	if len(got) != 1 || got[0] != "file-AbCdEf123456" {
		t.Fatalf("got %#v", got)
	}
}

func TestExtractDownloadAttachmentFileIDsAllowsImageAttachmentWithoutSize(t *testing.T) {
	body := []byte(`{
		"mapping": {
			"reply": {
				"message": {
					"metadata": {
						"attachments": [
							{"id":"file-no-size-123","name":"original.png","mime_type":"image/png"}
						]
					}
				}
			}
		}
	}`)
	got := extractDownloadAttachmentFileIDs(body, "2160x3840")
	if len(got) != 1 || got[0] != "file-no-size-123" {
		t.Fatalf("got %#v", got)
	}
}

func TestExtractDownloadAttachmentFileIDsAllowsMismatchedSizeMetadata(t *testing.T) {
	body := []byte(`{
		"attachments": [
			{"file_id":"file_00000000eeeeeeeeeeeeeeeeeeeeeeee","name":"original-1254x1254.png","mime_type":"image/png","width":1254,"height":1254}
		]
	}`)
	got := extractDownloadAttachmentFileIDs(body, "1024x1024")
	if len(got) != 1 || got[0] != "file_00000000eeeeeeeeeeeeeeeeeeeeeeee" {
		t.Fatalf("got %#v", got)
	}
}

func TestExtractDownloadAttachmentFileIDsIgnoresPreviewPointerWindow(t *testing.T) {
	body := []byte(`{
		"content": "preview original-1254x1254.png via file-service://file_00000000ffffffffffffffffffffffff"
	}`)
	got := extractDownloadAttachmentFileIDs(body, "1024x1024")
	if len(got) != 0 {
		t.Fatalf("got %#v", got)
	}
}

func TestExtractDownloadAttachmentFileIDsAllowsDownloadURLWindow(t *testing.T) {
	body := []byte(`{
		"download_url": "https://chatgpt.com/backend-api/estuary/content/example.png",
		"asset_pointer": "file-service://file_00000000ffffffffffffffffffffffff"
	}`)
	got := extractDownloadAttachmentFileIDs(body, "1024x1024")
	if len(got) != 1 || got[0] != "file_00000000ffffffffffffffffffffffff" {
		t.Fatalf("got %#v", got)
	}
}

func TestExtractSandboxDownloadsFindsMessageScopedImageLink(t *testing.T) {
	body := []byte(`{
		"mapping": {
			"reply": {
				"message": {
					"id": "msg-123",
					"author": {"role":"assistant"},
					"content": {
						"content_type": "text",
						"parts": ["已保存 PNG 原图：\\n[下载 cat-1024.png](sandbox:/workspace/scratch/abc/cat-1024.png)"]
					}
				}
			}
		}
	}`)
	got := extractSandboxDownloads(body, "1024x1024")
	if len(got) != 1 {
		t.Fatalf("want 1 sandbox download, got %d %#v", len(got), got)
	}
	if got[0].MessageID != "msg-123" || got[0].Path != "/workspace/scratch/abc/cat-1024.png" {
		t.Fatalf("got %#v", got[0])
	}
}

func TestExtractSandboxDownloadsPrefersTargetSizeText(t *testing.T) {
	body := []byte(`{
		"mapping": {
			"a": {"message": {"id":"msg-a", "content":{"parts":["[下载 small](sandbox:/workspace/scratch/a/small.png)"]}}},
			"b": {"message": {"id":"msg-b", "content":{"parts":["2160×3840 原图 [下载](sandbox:/workspace/scratch/b/original.png)"]}}}
		}
	}`)
	got := extractSandboxDownloads(body, "2160x3840")
	if len(got) != 1 || got[0].MessageID != "msg-b" || got[0].Path != "/workspace/scratch/b/original.png" {
		t.Fatalf("got %#v", got)
	}
}

func TestNormalizeThinkingEffortAllowsMin(t *testing.T) {
	if got := normalizeThinkingEffort("min"); got != "min" {
		t.Fatalf("got %q", got)
	}
	if !isWorkModeModel("gpt-5.6-luna-wm") {
		t.Fatalf("expected -wm model to enable work-mode payload")
	}
}
