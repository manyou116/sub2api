package service

import (
	"fmt"
	"slices"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ImageShadowModel describes the small, explicit model surface exposed by an
// image shadow. Keeping this catalog in the service prevents the UI from
// inventing execution models or thinking values.
type ImageShadowModel struct {
	ID                    string   `json:"id"`
	Label                 string   `json:"label"`
	Modes                 []string `json:"modes"`
	ThinkingEfforts       []string `json:"thinking_efforts"`
	DefaultThinkingEffort string   `json:"default_thinking_effort"`
	Recommended           bool     `json:"recommended"`
	Verified              bool     `json:"verified"`
}

type ImageShadowModelCatalog struct {
	Models      []ImageShadowModel `json:"models"`
	Recommended map[string]string  `json:"recommended"`
}

const (
	imageShadowModelRecommended = "recommended"
	imageShadowModelFixed       = "fixed"
	imageShadowModelAuto        = "auto"
)

// Catalog options are taken from the upstream model directory. Verified marks
// a successful image result, not a provider billing attribution guarantee.
var imageShadowModelCatalog = ImageShadowModelCatalog{
	Models: []ImageShadowModel{
		{ID: "gpt-5-6-thinking", Label: "GPT-5.6 Sol", Modes: []string{ImageShadowModeWeb}, ThinkingEfforts: []string{"standard", "extended"}, DefaultThinkingEffort: "standard", Recommended: true, Verified: true},
		{ID: "gpt-5.6-luna-wm", Label: "GPT-5.6 Luna WM", Modes: []string{ImageShadowModeWork}, ThinkingEfforts: []string{"standard", "extended"}, DefaultThinkingEffort: "standard", Recommended: true, Verified: true},
		{ID: imageShadowModelAuto, Label: "上游自动选择", Modes: []string{ImageShadowModeWeb}, ThinkingEfforts: []string{}, DefaultThinkingEffort: "", Verified: true},
	},
	Recommended: map[string]string{ImageShadowModeWeb: "gpt-5-6-thinking", ImageShadowModeWork: "gpt-5.6-luna-wm"},
}

func GetImageShadowModelCatalog() ImageShadowModelCatalog {
	result := ImageShadowModelCatalog{Recommended: map[string]string{}}
	result.Models = append(result.Models, imageShadowModelCatalog.Models...)
	for key, value := range imageShadowModelCatalog.Recommended {
		result.Recommended[key] = value
	}
	return result
}

func imageShadowMode(account *Account) string {
	if account != nil && account.IsWorkImageShadow() {
		return ImageShadowModeWork
	}
	return ImageShadowModeWeb
}

func imageShadowRecommendedModel(mode string) string {
	if model := imageShadowModelCatalog.Recommended[mode]; model != "" {
		return model
	}
	return imageShadowModelAuto
}

func imageShadowModel(mode, id string) (ImageShadowModel, bool) {
	for _, model := range imageShadowModelCatalog.Models {
		if model.ID == id && slices.Contains(model.Modes, mode) {
			return model, true
		}
	}
	return ImageShadowModel{}, false
}

func validateImageShadowOptions(value any, mode string) error {
	if value == nil {
		return nil
	}
	options, ok := value.(map[string]any)
	if !ok {
		return infraerrors.BadRequest("IMAGE_SHADOW_OPTIONS_INVALID", "image shadow settings must be an object")
	}
	invalid := func() error {
		return infraerrors.BadRequest("IMAGE_SHADOW_OPTIONS_INVALID", fmt.Sprintf("invalid %s image shadow model settings", mode))
	}
	modelMode := imageShadowModelRecommended
	if raw, exists := options["model_mode"]; exists {
		var ok bool
		modelMode, ok = raw.(string)
		if !ok || (modelMode != imageShadowModelRecommended && modelMode != imageShadowModelFixed) {
			return invalid()
		}
	}
	if raw, exists := options["model"]; exists {
		model, ok := raw.(string)
		if !ok || len(model) > 200 || strings.ContainsAny(model, "\r\n") {
			return invalid()
		}
		if modelMode == imageShadowModelFixed {
			if _, found := imageShadowModel(mode, model); !found {
				return invalid()
			}
		}
	}
	if modelMode == imageShadowModelFixed {
		model, ok := options["model"].(string)
		if !ok || strings.TrimSpace(model) == "" {
			return invalid()
		}
	}
	if raw, exists := options["thinking_effort"]; exists {
		effort, ok := raw.(string)
		if !ok || len(effort) > 32 || strings.ContainsAny(effort, "\r\n") {
			return invalid()
		}
		if modelMode == imageShadowModelFixed {
			model, _ := options["model"].(string)
			entry, found := imageShadowModel(mode, model)
			if found && effort != "" && !slices.Contains(entry.ThinkingEfforts, effort) {
				return invalid()
			}
		}
	}
	if raw, exists := options["require_download_attachment"]; exists {
		requireDownload, ok := raw.(bool)
		if !ok || (mode == ImageShadowModeWork && !requireDownload) {
			return invalid()
		}
	}
	for key := range options {
		if key != "model_mode" && key != "model" && key != "thinking_effort" && key != "require_download_attachment" {
			return invalid()
		}
	}
	return nil
}

func validateResolvedImageShadowOptions(options webImageOptions, mode string) error {
	values := map[string]any{"model_mode": "fixed", "model": options.Model, "thinking_effort": options.ThinkingEffort}
	if options.RequireDownloadAttachment != nil {
		values["require_download_attachment"] = *options.RequireDownloadAttachment
	}
	return validateImageShadowOptions(values, mode)
}

func imageShadowCodexModel(model string) string {
	return normalizeCodexModel(strings.TrimSuffix(model, "-wm"))
}

func imageShadowQuotaSource(account *Account) string {
	if account.IsWorkImageShadow() {
		return "parent_codex"
	}
	return "parent_web_images"
}

func imageShadowRequestedMode(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-image-2-web":
		return ImageShadowModeWeb
	case "gpt-image-2-work":
		return ImageShadowModeWork
	default:
		return ""
	}
}

func imageShadowModelModeMismatch(account *Account, requestedModel string) bool {
	mode := imageShadowRequestedMode(requestedModel)
	return mode != "" && (!account.IsImageShadow() || imageShadowMode(account) != mode)
}
