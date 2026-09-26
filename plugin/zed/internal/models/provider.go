package models

import (
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	zedconfig "github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/zed"
)

// Provider implements pluginapi.ModelProvider. All models are OAuth-scoped:
// the account's catalog comes from GET /models, minted per organization.
type Provider struct {
	settings zedconfig.Settings
	tokens   *zed.TokenPool
}

func New(settings zedconfig.Settings, tokens *zed.TokenPool) *Provider {
	return &Provider{settings: settings, tokens: tokens}
}

func (p *Provider) StaticModels(context.Context, pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: credentials.Provider}, nil
}

func (p *Provider) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	storage, errParse := credentials.Parse(req.StorageJSON, p.settings)
	if errParse != nil {
		return pluginapi.ModelResponse{}, errParse
	}
	if storage == nil {
		return pluginapi.ModelResponse{}, fmt.Errorf("Zed auth storage is missing")
	}
	llmToken, errToken := p.tokens.LLMToken(ctx, req.HTTPClient, p.settings, storage.UserID, storage.AccessToken, storage.SystemID, storage.OrganizationID, storage.Key())
	if errToken != nil {
		return pluginapi.ModelResponse{}, fmt.Errorf("mint Zed LLM token: %w", errToken)
	}
	catalog, errModels := zed.FetchModels(ctx, req.HTTPClient, p.settings, llmToken)
	if errModels != nil {
		return pluginapi.ModelResponse{}, errModels
	}
	models := make([]pluginapi.ModelInfo, 0, len(catalog.Models))
	for _, model := range catalog.Models {
		if strings.TrimSpace(model.ID) == "" {
			continue
		}
		info := pluginapi.ModelInfo{
			ID:                  model.ID,
			Object:              "model",
			OwnedBy:             model.Provider,
			DisplayName:         model.DisplayName,
			Name:                model.ID,
			ContextLength:       model.MaxTokenCount,
			InputTokenLimit:     model.MaxTokenCount,
			MaxCompletionTokens: model.MaxOutputTokens,
			OutputTokenLimit:    model.MaxOutputTokens,
		}
		if model.IsDisabled {
			info.Description = "Disabled by Zed"
			if model.DisabledReason != "" {
				info.Description = "Disabled by Zed: " + model.DisabledReason
			}
		}
		models = append(models, info)
	}
	return pluginapi.ModelResponse{Provider: credentials.Provider, Models: models}, nil
}
