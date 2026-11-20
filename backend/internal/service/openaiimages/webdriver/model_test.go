package webdriver

import "testing"

func TestImageModelSlugPreservesAutoAndExplicitModels(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{input: "auto", want: "auto"},
		{input: " auto ", want: "auto"},
		{input: "AUTO", want: "auto"},
		{input: "gpt-5-6-thinking", want: "gpt-5-6-thinking"},
		{input: " gpt-5.6-luna-wm ", want: "gpt-5.6-luna-wm"},
		{input: "", want: "gpt-5-6-thinking"},
		{input: "gpt-image-2", want: "gpt-5-6-thinking"},
	} {
		t.Run(tt.input, func(t *testing.T) {
			if got := imageModelSlug(tt.input); got != tt.want {
				t.Fatalf("imageModelSlug(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
