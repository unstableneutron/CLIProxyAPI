package configaccess

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestConfiguredExtraAPIKeyHeaders(t *testing.T) {
	t.Cleanup(func() { Register(nil) })
	for _, tc := range []struct {
		name       string
		config     string
		headers    map[string]string
		wantSource string
		wantError  sdkaccess.AuthErrorCode
	}{
		{"header only", "extra-api-key-auth-headers: [ChatGPT-Account-Id]", map[string]string{"chatgpt-account-id": "test-key"}, "chatgpt-account-id", ""},
		{"invalid bearer with valid extra header", "extra-api-key-auth-headers: [ChatGPT-Account-Id]", map[string]string{"Authorization": "Bearer upstream-token", "ChatGPT-Account-Id": "test-key"}, "chatgpt-account-id", ""},
		{"unknown account", "extra-api-key-auth-headers: [ChatGPT-Account-Id]", map[string]string{"ChatGPT-Account-Id": "unknown-account"}, "", sdkaccess.AuthErrorCodeInvalidCredential},
		{"not opted in", "", map[string]string{"ChatGPT-Account-Id": "test-key"}, "", sdkaccess.AuthErrorCodeNoCredentials},
		{"wrong header", "extra-api-key-auth-headers: [ChatGPT-Account-Id]", map[string]string{"ChatGPT-AccountId": "test-key"}, "", sdkaccess.AuthErrorCodeNoCredentials},
		{"missing credentials", "extra-api-key-auth-headers: [ChatGPT-Account-Id]", nil, "", sdkaccess.AuthErrorCodeNoCredentials},
		{"bearer still works", "extra-api-key-auth-headers: [ChatGPT-Account-Id]", map[string]string{"Authorization": "Bearer test-key", "ChatGPT-Account-Id": "unknown-account"}, "authorization", ""},
		{"second configured header", "extra-api-key-auth-headers: [X-Other-Key, ' ChatGPT-Account-Id ']", map[string]string{"ChatGPT-Account-Id": "test-key"}, "chatgpt-account-id", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := sdkconfig.ParseConfigBytes([]byte("api-keys: [test-key]\n" + tc.config + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			Register(&cfg.SDKConfig)
			providers := sdkaccess.RegisteredProviders()
			if len(providers) != 1 {
				t.Fatalf("providers = %d, want 1", len(providers))
			}
			req := httptest.NewRequest("GET", "/v1/responses", nil)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}
			result, authErr := providers[0].Authenticate(context.Background(), req)
			if tc.wantError != "" {
				if authErr == nil || authErr.Code != tc.wantError || result != nil {
					t.Fatalf("result = %v, error = %v, want %s", result, authErr, tc.wantError)
				}
				return
			}
			if authErr != nil || result == nil {
				t.Fatalf("authentication failed: %v", authErr)
			}
			if result.Principal != "test-key" || result.Metadata["source"] != tc.wantSource {
				t.Fatalf("unexpected identity or source: %+v", result)
			}
			for name, value := range tc.headers {
				if req.Header.Get(name) != value {
					t.Fatalf("authentication modified header %s", name)
				}
			}
		})
	}
}

func TestHeaderRedactionSurvivesAuthReload(t *testing.T) {
	t.Cleanup(func() { Register(nil) })
	Register(&sdkconfig.SDKConfig{APIKeys: []string{"test-key"}, ExtraAPIKeyAuthHeaders: []string{"X-Retired-Credential"}})
	oldProvider := sdkaccess.RegisteredProviders()[0]
	Register(&sdkconfig.SDKConfig{APIKeys: []string{"test-key"}})
	req := httptest.NewRequest("GET", "/v1/responses", nil)
	req.Header.Set("X-Retired-Credential", "test-key")
	if _, err := oldProvider.Authenticate(context.Background(), req); err != nil {
		t.Fatalf("old in-flight provider stopped accepting its credential: %v", err)
	}
	if _, err := sdkaccess.RegisteredProviders()[0].Authenticate(context.Background(), req); err == nil {
		t.Fatal("replacement provider still accepts removed header")
	}
	loggedHeaders := req.Header.Clone()
	util.RedactExtraAPIKeyAuthHeaders(loggedHeaders)
	if loggedHeaders.Get("X-Retired-Credential") != "[REDACTED]" || util.MaskSensitiveHeaderValue("X-Retired-Credential", "test-key") != "[REDACTED]" {
		t.Fatal("reload exposed a retired credential header in logs")
	}
	if req.Header.Get("X-Retired-Credential") != "test-key" {
		t.Fatal("redaction modified live headers")
	}
}
