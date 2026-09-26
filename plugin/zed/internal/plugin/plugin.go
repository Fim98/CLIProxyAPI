package plugin

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/auth"
	zedconfig "github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/executor"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/models"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/zed"
)

type ZedPlugin struct {
	auth     *auth.Provider
	models   *models.Provider
	executor *executor.Executor
	webLogin *auth.WebLogin
}

// SetAuthSaver bridges the host's auth-dir save callback from the ABI layer.
// It must be called before Build registers the plugin with the host.
func SetAuthSaver(saver auth.AuthSaver) {
	auth.SetAuthSaver(saver)
}

// Build assembles the plugin from the raw config_yaml subtree the host
// supplies for plugins.configs.zed.
func Build(configYAML []byte) pluginapi.Plugin {
	settings := zedconfig.Parse(configYAML)
	tokens := zed.NewTokenPool()
	authProvider := auth.New(settings, tokens)
	p := &ZedPlugin{
		auth:     authProvider,
		models:   models.New(settings, tokens),
		executor: executor.New(settings, tokens),
		webLogin: auth.NewWebLogin(authProvider),
	}
	return pluginapi.Plugin{
		Metadata: pluginapi.Metadata{
			Name:             "Zed Provider",
			Version:          "0.1.0",
			Author:           "liyunfu",
			GitHubRepository: "https://github.com/liyunfu/cpa-plugin-zed",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "server-url", Type: pluginapi.ConfigFieldTypeString, Description: "Zed sign-in server base URL (default https://zed.dev)."},
				{Name: "cloud-url", Type: pluginapi.ConfigFieldTypeString, Description: "Zed cloud API base URL (default https://cloud.zed.dev)."},
				{Name: "client-version", Type: pluginapi.ConfigFieldTypeString, Description: "Zed app version reported in User-Agent and x-zed-version."},
				{Name: "organization-id", Type: pluginapi.ConfigFieldTypeString, Description: "Only save credentials for this organization id at login. Unset saves one credential per organization."},
				{Name: "oauth-callback-port", Type: pluginapi.ConfigFieldTypeInteger, Description: "Fixed 127.0.0.1 port for the sign-in callback, for remote hosts behind an SSH tunnel. Unset takes an ephemeral port."},
			},
		},
		Capabilities: pluginapi.Capabilities{
			AuthProvider:          p,
			ModelProvider:         p,
			Executor:              p,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  append([]string(nil), executor.InputFormats...),
			ExecutorOutputFormats: append([]string(nil), executor.OutputFormats...),
			CommandLinePlugin:     p,
			ManagementAPI:         p,
		},
	}
}

func (p *ZedPlugin) Identifier() string { return credentials.Provider }

func (p *ZedPlugin) ParseAuth(ctx context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	return p.auth.ParseAuth(ctx, req)
}

func (p *ZedPlugin) StartLogin(ctx context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	return p.auth.StartLogin(ctx, req)
}

func (p *ZedPlugin) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return p.auth.PollLogin(ctx, req)
}

func (p *ZedPlugin) RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	return p.auth.RefreshAuth(ctx, req)
}

func (p *ZedPlugin) StaticModels(ctx context.Context, req pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return p.models.StaticModels(ctx, req)
}

func (p *ZedPlugin) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	return p.models.ModelsForAuth(ctx, req)
}

func (p *ZedPlugin) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return p.executor.Execute(ctx, req)
}

func (p *ZedPlugin) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return p.executor.ExecuteStream(ctx, req)
}

func (p *ZedPlugin) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return p.executor.CountTokens(ctx, req)
}

func (p *ZedPlugin) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return p.executor.HttpRequest(ctx, req)
}

func (p *ZedPlugin) RegisterCommandLine(ctx context.Context, req pluginapi.CommandLineRegistrationRequest) (pluginapi.CommandLineRegistrationResponse, error) {
	return p.auth.RegisterCommandLine(ctx, req)
}

func (p *ZedPlugin) ExecuteCommandLine(ctx context.Context, req pluginapi.CommandLineExecutionRequest) (pluginapi.CommandLineExecutionResponse, error) {
	return p.auth.ExecuteCommandLine(ctx, req)
}

func (p *ZedPlugin) RegisterManagement(ctx context.Context, req pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	return p.webLogin.RegisterManagement(ctx, req)
}

func (p *ZedPlugin) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	return p.webLogin.HandleManagement(ctx, req)
}

var _ pluginapi.AuthProvider = (*ZedPlugin)(nil)
var _ pluginapi.ModelProvider = (*ZedPlugin)(nil)
var _ pluginapi.ProviderExecutor = (*ZedPlugin)(nil)
var _ pluginapi.CommandLinePlugin = (*ZedPlugin)(nil)
var _ pluginapi.ManagementAPI = (*ZedPlugin)(nil)
