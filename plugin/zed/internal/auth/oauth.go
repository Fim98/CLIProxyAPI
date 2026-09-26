package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	zedconfig "github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/config"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/credentials"
	"github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/zed"
)

const (
	loginTimeout = 15 * time.Minute
	// finalizeTimeout bounds the profile fetch and organization expansion that
	// run inside the poll that observes the callback.
	finalizeTimeout = 60 * time.Second
)

// loginSession tracks one in-flight browser sign-in.
type loginSession struct {
	state      string
	privateKey *rsa.PrivateKey
	capture    *loopbackCapture
	systemID   string
	expiresAt  time.Time
	// signInURL is the real zed.dev /native_app_signin URL the relay page
	// opens; settings snapshots the config this login started with.
	signInURL string
	settings  zedconfig.Settings

	mu     sync.Mutex
	auths  []pluginapi.AuthData
	result error
	done   bool
}

func (s *loginSession) expired() bool {
	return time.Now().After(s.expiresAt)
}

// outcome reports the one-shot result of a completed session so later polls
// return the same answer without repeating the work.
func (s *loginSession) outcome() ([]pluginapi.AuthData, error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auths, s.result, s.done
}

func (s *loginSession) recordOutcome(auths []pluginapi.AuthData, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auths, s.result, s.done = auths, err, true
}

// oauthCoordinator owns the in-flight login sessions. CPA polls PollLogin
// frequently, so the blocking work runs exactly once per session.
type oauthCoordinator struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
}

func newOAuthCoordinator() *oauthCoordinator {
	return &oauthCoordinator{sessions: make(map[string]*loginSession)}
}

func (c *oauthCoordinator) pruneLocked() {
	now := time.Now()
	for state, session := range c.sessions {
		if now.After(session.expiresAt) {
			session.capture.Close()
			delete(c.sessions, state)
		}
	}
}

func (c *oauthCoordinator) get(state string) (*loginSession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, ok := c.sessions[state]
	return session, ok
}

func (c *oauthCoordinator) remove(state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if session, ok := c.sessions[state]; ok {
		session.capture.Close()
		delete(c.sessions, state)
	}
}

// start begins a browser sign-in: bind the loopback listener, generate the RSA
// keypair whose public half the web app will encrypt the token to, and hand
// back the /native_app_signin URL.
func (c *oauthCoordinator) start(settings zedconfig.Settings) (*loginSession, string, error) {
	c.mu.Lock()
	c.pruneLocked()
	c.mu.Unlock()

	publicKey, privateKey, errKeypair := zed.GenerateLoginKeypair()
	if errKeypair != nil {
		return nil, "", errKeypair
	}
	state, errState := randomState()
	if errState != nil {
		return nil, "", fmt.Errorf("generate Zed login state: %w", errState)
	}
	capture, errListen := startLoopbackCapture(settings.OAuthCallbackPort, settings.ServerURL+"/native_app_signin_succeeded")
	if errListen != nil {
		return nil, "", errListen
	}
	systemID := uuid.NewString()
	port := listenerPort(capture)
	signInURL := settings.ServerURL + "/native_app_signin?" + url.Values{
		"native_app_port":       []string{strconv.Itoa(port)},
		"native_app_public_key": []string{publicKey},
		"system_id":             []string{systemID},
	}.Encode()
	session := &loginSession{
		state:      state,
		privateKey: privateKey,
		capture:    capture,
		systemID:   systemID,
		expiresAt:  time.Now().Add(loginTimeout),
		signInURL:  signInURL,
		settings:   settings,
	}

	c.mu.Lock()
	c.sessions[state] = session
	c.mu.Unlock()
	return session, signInURL, nil
}

// poll checks a login session. It returns pending while the browser has not
// yet called back; once the callback lands it decrypts the credential and
// expands it into per-organization auths.
func (c *oauthCoordinator) poll(ctx context.Context, session *loginSession, settings zedconfig.Settings, captured callbackResult, hasResult bool, proxyURL string, httpClient pluginapi.HostHTTPClient) ([]pluginapi.AuthData, error, bool) {
	if auths, result, done := session.outcome(); done {
		return auths, result, true
	}
	if !hasResult {
		if session.expired() {
			c.remove(session.state)
			return nil, fmt.Errorf("Zed sign-in timed out; start the login again"), true
		}
		return nil, nil, false
	}
	auths, errFinalize := expandAndStore(ctx, session, settings, captured, proxyURL, httpClient)
	session.recordOutcome(auths, errFinalize)
	c.remove(session.state)
	if errFinalize != nil {
		return nil, errFinalize, true
	}
	return auths, nil, true
}

// expandAndStore decrypts the access token and produces one auth per Zed
// organization, with the default organization first.
func expandAndStore(ctx context.Context, session *loginSession, settings zedconfig.Settings, captured callbackResult, proxyURL string, httpClient pluginapi.HostHTTPClient) ([]pluginapi.AuthData, error) {
	if captured.errorText != "" {
		return nil, fmt.Errorf("%s", captured.errorText)
	}
	accessToken, errDecrypt := zed.DecryptAccessToken(session.privateKey, captured.accessToken)
	if errDecrypt != nil {
		return nil, errDecrypt
	}

	finalizeCtx, cancelFinalize := context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
	defer cancelFinalize()
	profile, errProfile := zed.FetchUserProfile(finalizeCtx, httpClient, settings, captured.userID, accessToken, session.systemID)
	if errProfile != nil {
		return nil, fmt.Errorf("validate Zed sign-in: %w", errProfile)
	}

	organizations := selectOrganizations(profile, settings.OrganizationID)
	if len(organizations) == 0 {
		return nil, fmt.Errorf("Zed account has no usable organization (requested %q)", settings.OrganizationID)
	}

	now := time.Now()
	auths := make([]pluginapi.AuthData, 0, len(organizations))
	for _, organization := range organizations {
		organizationName := organization.Name
		if organizationName == "" && organization.IsPersonal {
			organizationName = "Personal"
		}
		storage := credentials.Storage{
			Type:             credentials.Provider,
			UserID:           captured.userID,
			AccessToken:      accessToken,
			SystemID:         session.systemID,
			Username:         profile.Username,
			Email:            profile.Email,
			OrganizationID:   organization.ID,
			OrganizationName: organizationName,
			Plan:             profile.PlansByOrganization[organization.ID],
			ServerURL:        settings.ServerURL,
			LastRefresh:      now.UTC().Format(time.RFC3339),
		}
		storage.Raw = map[string]any{}
		if proxyURL != "" {
			storage.Raw["proxy_url"] = proxyURL
		}
		if errValidate := storage.Validate(); errValidate != nil {
			return nil, errValidate
		}
		fileName := storage.DefaultAuthFileName()
		auths = append(auths, storage.AuthData(fileName, fileName, now.Add(credentials.ProfileRefreshInterval)))
	}
	return auths, nil
}

// selectOrganizations orders the account's organizations with the default one
// first, optionally narrowed to a configured organization id.
func selectOrganizations(profile *zed.UserProfile, requestedID string) []zed.Organization {
	organizations := profile.Organizations
	if requestedID != "" {
		matched := make([]zed.Organization, 0, 1)
		for _, organization := range organizations {
			if strings.EqualFold(organization.ID, requestedID) {
				matched = append(matched, organization)
			}
		}
		return matched
	}
	for index, organization := range organizations {
		if organization.ID == profile.DefaultOrganizationID && index > 0 {
			rest := append(organizations[:index:index], organizations[index+1:]...)
			organizations = append([]zed.Organization{organization}, rest...)
			break
		}
	}
	return organizations
}

func randomState() (string, error) {
	buffer := make([]byte, 18)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return "", errRead
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
