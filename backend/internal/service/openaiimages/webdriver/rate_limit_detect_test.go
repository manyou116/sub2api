package webdriver

import "testing"

func TestLooksLikeRateLimitMessage_FreePlan(t *testing.T) {
	msg := "You've hit the Free plan limit for image generations requests. You can create more images when the limit resets in 23 hours and 5 minutes. I was unable to invoke the image generation tool right now because you've reached the Free plan image generation limit. I can't generate the requested image until the limit resets or you upgrade to a plan with additional image generation capacity."
	if !looksLikeRateLimitMessage(msg) {
		t.Fatal("expected rate limit detection")
	}
	if looksLikePolicyMessage(msg) {
		t.Fatal("rate limit must not be classified as policy")
	}
	err := classifyHTTP("poll", 200, msg)
	if err == nil || err.Kind != ErrorKindRateLimited {
		t.Fatalf("classify kind=%v err=%v", err, err)
	}
}

func TestLooksLikePolicyStillWorks(t *testing.T) {
	msg := "This request violates our content policy and I cannot help with that."
	if !looksLikePolicyMessage(msg) {
		t.Fatal("expected policy")
	}
	if looksLikeRateLimitMessage(msg) {
		t.Fatal("policy should not be rate limit")
	}
}

func TestFindConversationPolicyErrorSkipsImagegenSkillDefinition(t *testing.T) {
	body := []byte(`{
		"mapping": {
			"skill": {
				"message": {
					"author": {"role": "assistant"},
					"content": {
						"content_type": "text",
						"parts": [
							"---\nname: \"imagegen\"\ndescription: \"Generate or edit raster images. Do not use when the task is better handled by SVG.\"\n---\n# Image Generation Skill\nNot allowed augmentations"
						]
					}
				}
			}
		}
	}`)
	if got := findConversationPolicyError(body); got != "" {
		t.Fatalf("skill definition should not be classified as policy: %q", got)
	}
}

func TestFindConversationPolicyErrorStillDetectsAssistantRefusal(t *testing.T) {
	body := []byte(`{
		"mapping": {
			"reply": {
				"message": {
					"author": {"role": "assistant"},
					"content": {
						"content_type": "text",
						"parts": ["This request violates our content policy and I cannot help with that."]
					}
				}
			}
		}
	}`)
	if got := findConversationPolicyError(body); got == "" {
		t.Fatal("expected assistant policy refusal")
	}
}
