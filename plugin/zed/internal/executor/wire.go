package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	zedconfig "github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/zed"
)

// Zed's cloud accepts a native provider request per model family
// (crates/language_models_cloud/src/language_models_cloud.rs): Anthropic
// Messages for anthropic, OpenAI Responses for open_ai, generateContent for
// google, and OpenAI Chat Completions for x_ai.
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "open_ai"
	ProviderGoogle    = "google"
	ProviderXAi       = "x_ai"
)

// wireFormatForProvider maps a Zed model provider to the CPA wire format of
// its native request shape.
func wireFormatForProvider(provider string) sdktranslator.Format {
	switch provider {
	case ProviderAnthropic:
		return sdktranslator.FormatClaude
	case ProviderOpenAI:
		return sdktranslator.FormatOpenAIResponse
	case ProviderGoogle:
		return sdktranslator.FormatGemini
	case ProviderXAi:
		return sdktranslator.FormatOpenAI
	}
	return ""
}

// inferProviderFromModel guesses the Zed provider from the model id when the
// account catalog is unavailable.
func inferProviderFromModel(model string) string {
	normalized := strings.ToLower(model)
	switch {
	case strings.HasPrefix(normalized, "claude"):
		return ProviderAnthropic
	case strings.HasPrefix(normalized, "gemini"):
		return ProviderGoogle
	case strings.HasPrefix(normalized, "grok"):
		return ProviderXAi
	case strings.HasPrefix(normalized, "gpt"),
		strings.HasPrefix(normalized, "o1"),
		strings.HasPrefix(normalized, "o3"),
		strings.HasPrefix(normalized, "o4"),
		strings.HasPrefix(normalized, "chatgpt"),
		strings.HasPrefix(normalized, "codex"):
		return ProviderOpenAI
	}
	return ""
}

// catalogCache remembers the model→provider mapping from GET /models per
// credential, so inference requests do not re-fetch the catalog.
type catalogCache struct {
	mu      sync.Mutex
	entries map[string]cachedCatalog
	ttl     time.Duration
}

type cachedCatalog struct {
	providers map[string]string
	fetchedAt time.Time
}

func newCatalogCache() *catalogCache {
	return &catalogCache{entries: make(map[string]cachedCatalog), ttl: 10 * time.Minute}
}

func (c *catalogCache) providerFor(key, model string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || time.Since(entry.fetchedAt) > c.ttl {
		return "", false
	}
	provider, ok := entry.providers[model]
	return provider, ok
}

func (c *catalogCache) store(key string, response *zed.ListModelsResponse) {
	if response == nil {
		return
	}
	providers := make(map[string]string, len(response.Models))
	for _, model := range response.Models {
		if model.ID != "" && model.Provider != "" {
			providers[model.ID] = model.Provider
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = cachedCatalog{providers: providers, fetchedAt: time.Now()}
}

// providerForModel resolves the Zed provider for a model through the account
// catalog, falling back to id-prefix inference when the catalog is cold or
// unreachable. Zed rejects a completion whose provider does not match the
// model's family, so the catalog result wins whenever it has the model.
func (e *Executor) providerForModel(ctx context.Context, storage credentials.Storage, model string, httpClient pluginapi.HostHTTPClient) (string, error) {
	if provider, ok := e.catalog.providerFor(storage.Key(), model); ok {
		return provider, nil
	}
	llmToken, errToken := e.tokens.LLMToken(ctx, httpClient, e.settings, storage.UserID, storage.AccessToken, storage.SystemID, storage.OrganizationID, storage.Key())
	if errToken != nil {
		// Without an LLM token there is no request to serve either; report it.
		return "", errToken
	}
	catalog, errModels := zed.FetchModels(ctx, httpClient, e.settings, llmToken)
	if errModels != nil {
		if provider := inferProviderFromModel(model); provider != "" {
			return provider, nil
		}
		return "", errModels
	}
	e.catalog.store(storage.Key(), catalog)
	if provider, ok := e.catalog.providerFor(storage.Key(), model); ok {
		return provider, nil
	}
	if provider := inferProviderFromModel(model); provider != "" {
		return provider, nil
	}
	return "", fmt.Errorf("model %q is not in the Zed catalog for this account", model)
}

var _ = zedconfig.Settings{}
