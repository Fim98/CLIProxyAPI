package auth

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	zedconfig "github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/zed"
)

// Provider implements pluginapi.AuthProvider for Zed accounts.
type Provider struct {
	settings zedconfig.Settings
	tokens   *zed.TokenPool
	oauth    *oauthCoordinator
}

// commandLineHTTPClient is the process-wide client for --zed-login, which has
// no host HTTP context available.
var cliHTTPClient = newCommandLineHTTPClient()

func New(settings zedconfig.Settings, tokens *zed.TokenPool) *Provider {
	return &Provider{settings: settings, tokens: tokens, oauth: newOAuthCoordinator()}
}

func (p *Provider) Identifier() string { return credentials.Provider }

func (p *Provider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	storage, errParse := credentials.Parse(req.RawJSON, p.settings)
	if errParse != nil {
		return pluginapi.AuthParseResponse{Handled: true}, errParse
	}
	if storage == nil {
		return pluginapi.AuthParseResponse{}, nil
	}
	auth := storage.AuthData(req.FileName, req.FileName, time.Now().Add(credentials.ProfileRefreshInterval))
	return pluginapi.AuthParseResponse{Handled: true, Auth: auth, Auths: []pluginapi.AuthData{auth}}, nil
}

// StartLogin opens a browser sign-in session and returns the URL the caller
// (Management Center or the --zed-login CLI) must open. It points at the
// plugin's own relay page, which opens the real Zed sign-in and captures the
// callback even when the browser runs on a different machine than CPA (for
// example a Render deployment): Zed redirects back to 127.0.0.1, and the page
// lets the user paste that URL straight back.
func (p *Provider) StartLogin(_ context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	settings := p.settings
	if value, ok := req.Metadata["server-url"].(string); ok {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			settings.ServerURL = trimmed
		}
	}
	session, _, errStart := p.oauth.start(settings)
	if errStart != nil {
		return pluginapi.AuthLoginStartResponse{}, errStart
	}
	pagePath := "/v0/resource/plugins/zed/login?state=" + url.QueryEscape(session.state)
	loginURL := pagePath
	if settings.PublicURL != "" {
		loginURL = settings.PublicURL + pagePath
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  credentials.Provider,
		URL:       loginURL,
		State:     session.state,
		ExpiresAt: session.expiresAt,
		Metadata: map[string]any{
			"flow":                "zed_relay_login",
			"organization_id":     settings.OrganizationID,
			"oauth-callback-port": settings.OAuthCallbackPort,
		},
	}, nil
}

// PollLogin returns pending until the browser callback lands, then validates
// the account and returns one auth per organization.
func (p *Provider) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	session, ok := p.oauth.get(req.State)
	if !ok {
		return pluginapi.AuthLoginPollResponse{}, fmt.Errorf("unknown or completed Zed login state")
	}
	var captured callbackResult
	hasResult := false
	select {
	case captured = <-session.capture.Results():
		hasResult = true
	default:
	}
	auths, result, settled := p.oauth.poll(ctx, session, p.settings, captured, hasResult, req.Host.ProxyURL, req.HTTPClient)
	if !settled {
		return pluginapi.AuthLoginPollResponse{Status: "pending", Message: "Waiting for the Zed browser sign-in to complete"}, nil
	}
	if result != nil {
		return pluginapi.AuthLoginPollResponse{}, result
	}
	if len(auths) == 0 {
		return pluginapi.AuthLoginPollResponse{}, fmt.Errorf("Zed sign-in produced no credentials")
	}
	response := pluginapi.AuthLoginPollResponse{
		Status:  "success",
		Auth:    auths[0],
		Auths:   auths,
		Message: fmt.Sprintf("Signed in as %s with %d organization(s)", auths[0].Label, len(auths)),
	}
	return response, nil
}

// RefreshAuth re-validates the long-lived credential and refreshes profile
// metadata. Zed has no refresh token: a 401 here means the user must sign in
// through the browser again.
func (p *Provider) RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	storage, errParse := credentials.Parse(req.StorageJSON, p.settings)
	if errParse != nil {
		return pluginapi.AuthRefreshResponse{}, errParse
	}
	if storage == nil {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("Zed auth storage is missing")
	}
	refreshCtx, cancelRefresh := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancelRefresh()
	profile, errProfile := zed.FetchUserProfile(refreshCtx, req.HTTPClient, p.settings, storage.UserID, storage.AccessToken, storage.SystemID)
	if errProfile != nil {
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("Zed credential check failed (sign in again if it keeps failing): %w", errProfile)
	}
	now := time.Now()
	storage.LastRefresh = now.UTC().Format(time.RFC3339)
	if profile.Username != "" {
		storage.Username = profile.Username
	}
	if profile.Email != "" {
		storage.Email = profile.Email
	}
	if storage.OrganizationID != "" {
		for _, organization := range profile.Organizations {
			if strings.EqualFold(organization.ID, storage.OrganizationID) {
				if organization.Name != "" {
					storage.OrganizationName = organization.Name
				}
				break
			}
		}
		if plan, ok := profile.PlansByOrganization[storage.OrganizationID]; ok {
			storage.Plan = plan
		}
	}
	next := now.Add(credentials.ProfileRefreshInterval)
	auth := storage.AuthData(req.AuthID, "", next)
	auth.FileName = ""
	mergeAuthMetadata(&auth, req.Metadata)
	mergeAuthAttributes(&auth, req.Attributes)
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: next}, nil
}

const refreshTimeout = 60 * time.Second

func mergeAuthMetadata(auth *pluginapi.AuthData, existing map[string]any) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	for key, value := range existing {
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "access_token", "credential_dir":
			continue
		}
		if _, present := auth.Metadata[key]; !present {
			auth.Metadata[key] = value
		}
	}
	auth.Metadata["type"] = credentials.Provider
	auth.Metadata["auth_kind"] = "oauth"
	delete(auth.Metadata, "credential_dir")
	delete(auth.Metadata, "credential_mode")
}

func mergeAuthAttributes(auth *pluginapi.AuthData, existing map[string]string) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	for key, value := range existing {
		if _, present := auth.Attributes[key]; !present {
			auth.Attributes[key] = value
		}
	}
	auth.Attributes["auth_kind"] = "oauth"
}

// RegisterCommandLine exposes --zed-login for interactive logins on the CPA host.
func (p *Provider) RegisterCommandLine(context.Context, pluginapi.CommandLineRegistrationRequest) (pluginapi.CommandLineRegistrationResponse, error) {
	return pluginapi.CommandLineRegistrationResponse{Flags: []pluginapi.CommandLineFlag{
		{Name: "zed-login", Usage: "Run Zed browser sign-in and save one credential per organization.", Type: "bool", DefaultValue: "false"},
		{Name: "zed-login-organization", Usage: "Restrict the saved credentials to this organization id.", Type: "string"},
	}}, nil
}

func (p *Provider) ExecuteCommandLine(ctx context.Context, req pluginapi.CommandLineExecutionRequest) (pluginapi.CommandLineExecutionResponse, error) {
	if !flagBool(req.TriggeredFlags, "zed-login") {
		return pluginapi.CommandLineExecutionResponse{}, nil
	}
	settings := p.settings
	if value := flagString(req.Flags, "zed-login-organization"); value != "" {
		settings.OrganizationID = value
	}
	session, signInURL, errStart := p.oauth.start(settings)
	if errStart != nil {
		return pluginapi.CommandLineExecutionResponse{}, errStart
	}
	stdout := openBrowserInstructions(signInURL)
	timeoutCtx, cancelTimeout := context.WithTimeout(context.WithoutCancel(ctx), loginTimeout)
	defer cancelTimeout()
	for {
		select {
		case <-timeoutCtx.Done():
			p.oauth.remove(session.state)
			return pluginapi.CommandLineExecutionResponse{Stdout: stdout, Stderr: []byte("Zed sign-in timed out\n"), ExitCode: 1}, nil
		case captured := <-session.capture.Results():
			auths, errFinalize, _ := p.oauth.poll(ctx, session, settings, captured, true, req.Host.ProxyURL, cliHTTPClient)
			if errFinalize != nil {
				return pluginapi.CommandLineExecutionResponse{Stdout: stdout, Stderr: []byte(errFinalize.Error() + "\n"), ExitCode: 1}, nil
			}
			stdout = append(stdout, []byte(fmt.Sprintf("Signed in; saved %d credential(s):\n", len(auths)))...)
			for _, auth := range auths {
				stdout = append(stdout, []byte("  - "+auth.FileName+" ("+auth.Label+")\n")...)
			}
			return pluginapi.CommandLineExecutionResponse{Stdout: stdout, Auths: auths}, nil
		}
	}
}

// openBrowserInstructions prints the sign-in URL and tries to open a local
// browser. Failing to open one is not fatal: the URL is in the output either
// way.
func openBrowserInstructions(signInURL string) []byte {
	var openErr error
	switch runtime.GOOS {
	case "darwin":
		openErr = exec.Command("open", signInURL).Start()
	case "linux":
		openErr = exec.Command("xdg-open", signInURL).Start()
	case "windows":
		openErr = exec.Command("rundll32", "url.dll,FileProtocolHandler", signInURL).Start()
	}
	message := "Open this URL to sign in to Zed:\n  " + signInURL + "\n"
	if openErr != nil {
		message += "(could not open a browser automatically; open the URL manually)\n"
	}
	return []byte(message + "Waiting for the browser callback...\n")
}

func flagBool(flags map[string]pluginapi.CommandLineFlagValue, name string) bool {
	value, ok := flags[name]
	return ok && value.Set && strings.EqualFold(strings.TrimSpace(value.Value), "true")
}

func flagString(flags map[string]pluginapi.CommandLineFlagValue, name string) string {
	value, ok := flags[name]
	if !ok {
		return ""
	}
	return strings.TrimSpace(value.Value)
}
