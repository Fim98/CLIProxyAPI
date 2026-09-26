package config

import (
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	DefaultServerURL = "https://zed.dev"
	// DefaultCloudURL is what the official client derives from https://zed.dev
	// (crates/http_client/src/http_client.rs build_zed_llm_url).
	DefaultCloudURL = "https://cloud.zed.dev"
	// DefaultClientVersion mirrors the Zed release this plugin protocol-matches.
	// User-Agent and x-zed-version are built from it.
	DefaultClientVersion = "1.23.0"
)

// Settings is the plugin configuration accepted under plugins.configs.zed.
type Settings struct {
	ServerURL         string `yaml:"server-url"`
	CloudURL          string `yaml:"cloud-url"`
	ClientVersion     string `yaml:"client-version"`
	OrganizationID    string `yaml:"organization-id"`
	OAuthCallbackPort int    `yaml:"oauth-callback-port"`
	// PublicURL is the externally reachable origin of the CPA host (for
	// example https://my-app.onrender.com). When set, the browser login page
	// URL is built absolute; otherwise a relative path is returned, which the
	// Management Center resolves against its own origin.
	PublicURL string `yaml:"public-url"`
}

// Defaults returns the settings with environment overrides and defaults applied.
func Defaults() Settings {
	settings := Settings{
		ServerURL:     firstNonEmpty(envValue("ZED_SERVER_URL"), DefaultServerURL),
		CloudURL:      firstNonEmpty(envValue("ZED_CLOUD_URL"), DefaultCloudURL),
		ClientVersion: firstNonEmpty(envValue("ZED_CLIENT_VERSION"), DefaultClientVersion),
		OrganizationID: firstNonEmpty(
			envValue("ZED_ORGANIZATION_ID"),
			envValue("ZED_ORG_ID"),
		),
		PublicURL: envValue("ZED_PUBLIC_URL"),
	}
	if port, ok := envPort("ZED_OAUTH_CALLBACK_PORT"); ok {
		settings.OAuthCallbackPort = port
	}
	settings.normalize()
	return settings
}

// Parse reads the raw config_yaml subtree the host supplies for this plugin and
// applies environment overrides and defaults on top.
func Parse(raw []byte) Settings {
	settings := Defaults()
	if len(raw) > 0 {
		var parsed Settings
		if errUnmarshal := yaml.Unmarshal(raw, &parsed); errUnmarshal == nil {
			if value := strings.TrimSpace(parsed.ServerURL); value != "" {
				settings.ServerURL = value
			}
			if value := strings.TrimSpace(parsed.CloudURL); value != "" {
				settings.CloudURL = value
			}
			if value := strings.TrimSpace(parsed.ClientVersion); value != "" {
				settings.ClientVersion = value
			}
			if value := strings.TrimSpace(parsed.OrganizationID); value != "" {
				settings.OrganizationID = value
			}
			if value := strings.TrimSpace(parsed.PublicURL); value != "" {
				settings.PublicURL = value
			}
			if parsed.OAuthCallbackPort > 0 && parsed.OAuthCallbackPort <= 65535 {
				settings.OAuthCallbackPort = parsed.OAuthCallbackPort
			}
		}
	}
	settings.normalize()
	return settings
}

func (s *Settings) normalize() {
	s.ServerURL = strings.TrimRight(strings.TrimSpace(s.ServerURL), "/")
	s.CloudURL = strings.TrimRight(strings.TrimSpace(s.CloudURL), "/")
	s.ClientVersion = strings.TrimSpace(s.ClientVersion)
	s.OrganizationID = strings.TrimSpace(s.OrganizationID)
	s.PublicURL = strings.TrimRight(strings.TrimSpace(s.PublicURL), "/")
	if s.ServerURL == "" {
		s.ServerURL = DefaultServerURL
	}
	if s.CloudURL == "" {
		s.CloudURL = DefaultCloudURL
	}
	if s.ClientVersion == "" {
		s.ClientVersion = DefaultClientVersion
	}
}

func envValue(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func envPort(key string) (int, bool) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0, false
	}
	port, errParse := strconv.Atoi(value)
	if errParse != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
