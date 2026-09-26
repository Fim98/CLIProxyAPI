package credentials

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	zedconfig "github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/config"
)

const Provider = "zed"

const CurrentStorageVersion = 1

// ProfileRefreshInterval asks CPA to re-validate the long-lived browser
// credential periodically. Zed has no refresh token: re-auth means redoing the
// browser flow, so refresh only re-checks the credential and refreshes profile
// metadata.
const ProfileRefreshInterval = 6 * time.Hour

// Storage is the provider-owned credential payload persisted by CLIProxyAPI in
// auth-dir. Zed's browser sign-in yields a single long-lived access token; the
// per-organization LLM token is minted on demand and never persisted.
type Storage struct {
	StorageVersion   int            `json:"storage_version,omitempty"`
	Type             string         `json:"type"`
	UserID           string         `json:"user_id"`
	AccessToken      string         `json:"access_token"`
	SystemID         string         `json:"system_id,omitempty"`
	Username         string         `json:"username,omitempty"`
	Email            string         `json:"email,omitempty"`
	OrganizationID   string         `json:"organization_id,omitempty"`
	OrganizationName string         `json:"organization_name,omitempty"`
	Plan             string         `json:"plan,omitempty"`
	ServerURL        string         `json:"server_url,omitempty"`
	LastRefresh      string         `json:"last_refresh,omitempty"`
	Raw              map[string]any `json:"-"`
}

// FromSettings returns empty storage carrying public provider settings.
func FromSettings(settings zedconfig.Settings) Storage {
	return Storage{
		Type:      Provider,
		ServerURL: settings.ServerURL,
	}
}

// Parse recognizes and validates a Zed auth file. It returns (nil, nil) when
// the payload belongs to another provider so the host can try other parsers.
func Parse(raw []byte, defaults zedconfig.Settings) (*Storage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var probe map[string]any
	if errUnmarshal := json.Unmarshal(raw, &probe); errUnmarshal != nil {
		return nil, fmt.Errorf("decode Zed auth: %w", errUnmarshal)
	}
	if !strings.EqualFold(strings.TrimSpace(stringValue(probe["type"])), Provider) {
		return nil, nil
	}
	var storage Storage
	if errUnmarshal := json.Unmarshal(raw, &storage); errUnmarshal != nil {
		return nil, fmt.Errorf("decode Zed auth: %w", errUnmarshal)
	}
	storage.Raw = cloneMap(probe)
	if strings.TrimSpace(storage.ServerURL) == "" {
		storage.ServerURL = defaults.ServerURL
	}
	storage.applyDefaults()
	if errValidate := storage.Validate(); errValidate != nil {
		return nil, errValidate
	}
	return &storage, nil
}

func (s *Storage) applyDefaults() {
	s.StorageVersion = CurrentStorageVersion
	s.Type = Provider
	s.UserID = strings.TrimSpace(s.UserID)
	s.AccessToken = strings.TrimSpace(s.AccessToken)
	s.SystemID = strings.TrimSpace(s.SystemID)
	s.Username = strings.TrimSpace(s.Username)
	s.Email = strings.TrimSpace(s.Email)
	s.OrganizationID = strings.TrimSpace(s.OrganizationID)
	s.OrganizationName = strings.TrimSpace(s.OrganizationName)
	s.Plan = strings.TrimSpace(s.Plan)
	s.ServerURL = strings.TrimRight(strings.TrimSpace(s.ServerURL), "/")
	s.LastRefresh = strings.TrimSpace(s.LastRefresh)
	if s.ServerURL == "" {
		s.ServerURL = zedconfig.DefaultServerURL
	}
}

// Validate checks the complete credential payload without touching the network.
func (s Storage) Validate() error {
	if s.UserID == "" {
		return fmt.Errorf("Zed user id is missing")
	}
	if s.AccessToken == "" {
		return fmt.Errorf("Zed access token is missing")
	}
	return nil
}

func (s Storage) JSON() []byte {
	out := cloneMap(s.Raw)
	if out == nil {
		out = make(map[string]any)
	}
	out["storage_version"] = CurrentStorageVersion
	out["type"] = Provider
	out["user_id"] = s.UserID
	out["access_token"] = s.AccessToken
	setOrDelete(out, "system_id", s.SystemID)
	setOrDelete(out, "username", s.Username)
	setOrDelete(out, "email", s.Email)
	setOrDelete(out, "organization_id", s.OrganizationID)
	setOrDelete(out, "organization_name", s.OrganizationName)
	setOrDelete(out, "plan", s.Plan)
	setOrDelete(out, "server_url", s.ServerURL)
	setOrDelete(out, "last_refresh", s.LastRefresh)
	out["auth_kind"] = "oauth"
	raw, _ := json.Marshal(out)
	return raw
}

// Key identifies a credential for in-process caches. Organization is part of
// the key because the LLM token is minted per organization.
func (s Storage) Key() string {
	return strings.Join([]string{s.UserID, s.AccessToken, s.OrganizationID, s.ServerURL}, "\x00")
}

func (s Storage) AuthData(id, fileName string, nextRefresh time.Time) pluginapi.AuthData {
	fileName = filepath.Base(strings.TrimSpace(fileName))
	if fileName == "" || fileName == "." {
		fileName = s.DefaultAuthFileName()
	}
	if strings.TrimSpace(id) == "" {
		id = fileName
	}
	metadata := map[string]any{
		"storage_version":          CurrentStorageVersion,
		"type":                     Provider,
		"auth_kind":                "oauth",
		"user_id":                  s.UserID,
		"username":                 s.Username,
		"organization_id":          s.OrganizationID,
		"organization_name":        s.OrganizationName,
		"plan":                     s.Plan,
		"refresh_interval_seconds": int64(ProfileRefreshInterval / time.Second),
	}
	return pluginapi.AuthData{
		Provider:    Provider,
		ID:          id,
		FileName:    fileName,
		Label:       s.AuthLabel(),
		Prefix:      strings.TrimSpace(stringValue(s.Raw["prefix"])),
		ProxyURL:    strings.TrimSpace(stringValue(s.Raw["proxy_url"])),
		Disabled:    boolValue(s.Raw["disabled"]),
		StorageJSON: s.JSON(),
		Metadata:    metadata,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
		NextRefreshAfter: nextRefresh,
	}
}

// DefaultAuthFileName is per account AND per organization: the same Zed login
// produces one file per organization it belongs to, so usage is scoped
// correctly and the scheduler can rotate between them.
func (s Storage) DefaultAuthFileName() string {
	identity := safeFileComponent(s.Username)
	if identity == "" {
		identity = safeFileComponent(s.UserID)
	}
	fileName := "zed-" + identity
	if org := safeFileComponent(s.OrganizationID); org != "" {
		fileName += "-" + org
	}
	return fileName + ".json"
}

func (s Storage) AuthLabel() string {
	account := s.Username
	if account == "" {
		account = s.UserID
	}
	if s.OrganizationName != "" {
		return "Zed (" + account + " @ " + s.OrganizationName + ")"
	}
	return "Zed (" + account + ")"
}

func safeFileComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, char := range value {
		allowed := (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-' || char == '@'
		if allowed {
			builder.WriteRune(char)
		} else if builder.Len() > 0 && !strings.HasSuffix(builder.String(), "-") {
			builder.WriteByte('-')
		}
		if builder.Len() >= 64 {
			break
		}
	}
	return strings.Trim(builder.String(), ".-_")
}

func setOrDelete(values map[string]any, key, value string) {
	if value == "" {
		delete(values, key)
		return
	}
	values[key] = value
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}
