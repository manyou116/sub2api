package service

import (
	"context"
	"errors"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

var ErrImageShadowModeRequired = errors.New("mixed Web/WM image group requires gpt-image-2-web or gpt-image-2-work")

type imageShadowRouteKey struct{}

type imageShadowModeRepository interface {
	ConfiguredImageShadowModes(context.Context, *int64, bool) ([]string, error)
}

// PrepareImageShadowRoutingContext pins the configured resource pool once per
// request. Health, pause and quota changes must not silently change its mode.
// Only the generic gpt-image-2 route needs a tiny configuration query; explicit
// aliases and unrelated native image models do not materialize account records.
func (s *OpenAIGatewayService) PrepareImageShadowRoutingContext(ctx context.Context, groupID *int64, model string) (context.Context, error) {
	if _, prepared := ctx.Value(imageShadowRouteKey{}).(string); prepared {
		return ctx, nil
	}
	model = strings.ToLower(strings.TrimSpace(model))
	mode := imageShadowRequestedMode(model)
	if mode == "" && model != "gpt-image-2" {
		return ctx, nil
	}
	if mode == "" && s.webImages != nil && s.accountRepo != nil {
		repo, ok := s.accountRepo.(imageShadowModeRepository)
		if !ok {
			return ctx, errors.New("image shadow routing state unavailable")
		}
		includeGrouped := s.cfg != nil && s.cfg.RunMode == config.RunModeSimple
		if includeGrouped {
			groupID = nil
		}
		modes, err := repo.ConfiguredImageShadowModes(ctx, groupID, includeGrouped)
		if err != nil {
			return ctx, err
		}
		for _, candidate := range modes {
			if mode != "" && mode != candidate {
				return ctx, ErrImageShadowModeRequired
			}
			mode = candidate
		}
	}
	return context.WithValue(ctx, imageShadowRouteKey{}, mode), nil
}

func (s *OpenAIGatewayService) imageShadowRoutingContext(ctx context.Context, groupID *int64, model string) (context.Context, error) {
	return s.PrepareImageShadowRoutingContext(ctx, groupID, model)
}

func imageShadowRouteMismatch(ctx context.Context, account *Account, model string) bool {
	mode, _ := ctx.Value(imageShadowRouteKey{}).(string)
	if explicit := imageShadowRequestedMode(model); explicit != "" {
		if mode != "" && mode != explicit {
			return true
		}
		mode = explicit
	}
	return mode != "" && (!account.IsImageShadow() || imageShadowMode(account) != mode)
}
