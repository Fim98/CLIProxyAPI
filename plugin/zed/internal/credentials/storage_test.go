package credentials

import (
	"encoding/json"
	"testing"
	"time"

	zedconfig "github.com/router-for-me/CLIProxyAPIPlugins/zed/internal/config"
)

func defaults() zedconfig.Settings {
	return zedconfig.Parse(nil)
}

func TestParseRejectsForeignProvider(t *testing.T) {
	storage, errParse := Parse([]byte(`{"type":"mirasim","access_token":"x"}`), defaults())
	if errParse != nil {
		t.Fatalf("Parse() error = %v", errParse)
	}
	if storage != nil {
		t.Fatalf("Parse() handled a foreign provider: %+v", storage)
	}
}

func TestStorageRoundTripPerOrganization(t *testing.T) {
	storage := Storage{
		Type:             Provider,
		UserID:           "12345",
		AccessToken:      "secret",
		SystemID:         "system-id",
		Username:         "octocat",
		OrganizationID:   "org-abc",
		OrganizationName: "Acme Inc",
		Plan:             "zed_pro",
		ServerURL:        "https://zed.dev",
	}
	storage.Raw = map[string]any{}
	auth := storage.AuthData("", "", time.Now().Add(time.Hour))
	if auth.Provider != Provider {
		t.Fatalf("auth provider = %s", auth.Provider)
	}
	if auth.FileName != "zed-octocat-org-abc.json" {
		t.Fatalf("auth file name = %s", auth.FileName)
	}
	if auth.Label != "Zed (octocat @ Acme Inc)" {
		t.Fatalf("auth label = %s", auth.Label)
	}

	parsed, errParse := Parse(auth.StorageJSON, defaults())
	if errParse != nil {
		t.Fatalf("Parse() error = %v", errParse)
	}
	if parsed == nil || parsed.OrganizationID != "org-abc" || parsed.UserID != "12345" {
		t.Fatalf("round trip storage = %+v", parsed)
	}
}

func TestDefaultAuthFileNameWithoutOrganization(t *testing.T) {
	storage := Storage{Type: Provider, UserID: "42", AccessToken: "t", Username: "octo"}
	if name := storage.DefaultAuthFileName(); name != "zed-octo.json" {
		t.Fatalf("file name = %s", name)
	}
}

func TestJSONContainsNoUnexpectedSecrets(t *testing.T) {
	storage := Storage{Type: Provider, UserID: "1", AccessToken: "tok"}
	storage.Raw = map[string]any{"prefix": "myprefix"}
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(storage.JSON(), &payload); errUnmarshal != nil {
		t.Fatalf("JSON() invalid: %v", errUnmarshal)
	}
	if payload["access_token"] != "tok" || payload["type"] != Provider || payload["prefix"] != "myprefix" {
		t.Fatalf("payload = %v", payload)
	}
	if _, hasOrg := payload["organization_id"]; hasOrg {
		t.Fatal("empty organization id must be omitted")
	}
}

func TestValidateRequiresUserAndToken(t *testing.T) {
	storage := Storage{Type: Provider, AccessToken: "tok"}
	if errValidate := storage.Validate(); errValidate == nil {
		t.Fatal("Validate() accepted storage without user id")
	}
	storage.UserID = "1"
	if errValidate := storage.Validate(); errValidate != nil {
		t.Fatalf("Validate() error = %v", errValidate)
	}
}
