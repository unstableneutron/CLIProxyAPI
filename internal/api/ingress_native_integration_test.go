package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestNativeChatGPTBackendIngress is opt-in because it consumes the separately built plugin.
// Run it with CPA_CHATGPT_BACKEND_PLUGIN=/absolute/path/to/chatgpt-backend.so.
func TestNativeChatGPTBackendIngress(t *testing.T) {
	pluginBinary := os.Getenv("CPA_CHATGPT_BACKEND_PLUGIN")
	if pluginBinary == "" {
		t.Skip("CPA_CHATGPT_BACKEND_PLUGIN is not set")
	}
	payload := bytes.Repeat([]byte("upload-"), 128*1024)
	download := bytes.Repeat([]byte("download-"), 128*1024)
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if got, want := r.Method, http.MethodPatch; got != want {
			t.Errorf("method = %q, want %q", got, want)
		}
		if got, want := r.URL.EscapedPath(), "/backend-api/files/a%2Fb"; got != want {
			t.Errorf("escaped path = %q, want %q", got, want)
		}
		if got, want := r.URL.RawQuery, "cursor=a%2Bb&cursor=a+b"; got != want {
			t.Errorf("query = %q, want %q", got, want)
		}
		wantAuthorization := "Bearer stored-token"
		if r.Header.Get("ChatGPT-Account-ID") == "unmatched" {
			wantAuthorization = "Bearer frontend-key"
		}
		if got := r.Header.Get("Authorization"); got != wantAuthorization {
			t.Errorf("Authorization = %q, want %q", got, wantAuthorization)
		}
		if got := r.Header.Get("Upgrade"); got != "" {
			t.Errorf("Upgrade = %q, want empty", got)
		}
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil || !bytes.Equal(body, payload) {
			t.Errorf("upload mismatch: bytes=%d err=%v", len(body), errRead)
		}
		w.Header().Set("X-Upstream", "ok")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(download)
	}))
	defer upstream.Close()

	pluginsDir := t.TempDir()
	target := filepath.Join(pluginsDir, "chatgpt-backend"+pluginhost.PluginExtension(runtime.GOOS))
	copyFile(t, target, pluginBinary)
	rawConfig := fmt.Sprintf(`
api-keys:
  - frontend-key
plugins:
  enabled: true
  dir: %q
  configs:
    chatgpt-backend:
      enabled: true
      base-url: %q
`, pluginsDir, upstream.URL)
	cfg, errConfig := config.ParseConfigBytes([]byte(rawConfig))
	if errConfig != nil {
		t.Fatal(errConfig)
	}
	authManager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := authManager.Register(context.Background(), &coreauth.Auth{
		ID: "stored", Provider: "codex", Status: coreauth.StatusActive, CreatedAt: time.Unix(1, 0),
		Attributes: map[string]string{"chatgpt_account_id": "acct-123"}, Metadata: map[string]any{"access_token": "stored-token"},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	host := pluginhost.New()
	t.Cleanup(host.ShutdownAll)
	host.ApplyConfig(context.Background(), cfg)
	if !host.PluginRegistered("chatgpt-backend") {
		t.Fatal("native chatgpt-backend plugin was not registered")
	}
	server := NewServer(cfg, authManager, sdkaccess.NewManager(), filepath.Join(t.TempDir(), "config.yaml"), WithPluginHost(host))

	req := httptest.NewRequest(http.MethodPatch, "/backend-api/files/a%2Fb?cursor=a%2Bb&cursor=a+b", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer frontend-key")
	req.Header.Set("ChatGPT-Account-ID", "acct-123")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	recorder := httptest.NewRecorder()
	server.engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusAccepted || !bytes.Equal(recorder.Body.Bytes(), download) || recorder.Header().Get("X-Upstream") != "ok" || recorder.Header().Get("Connection") != "" {
		t.Fatalf("response status=%d bytes=%d headers=%#v", recorder.Code, recorder.Body.Len(), recorder.Header())
	}
	fallback := httptest.NewRequest(http.MethodPatch, "/backend-api/files/a%2Fb?cursor=a%2Bb&cursor=a+b", bytes.NewReader(payload))
	fallback.Header.Set("Authorization", "Bearer frontend-key")
	fallback.Header.Set("ChatGPT-Account-ID", "unmatched")
	fallbackRecorder := httptest.NewRecorder()
	server.engine.ServeHTTP(fallbackRecorder, fallback)
	if fallbackRecorder.Code != http.StatusAccepted {
		t.Fatalf("fallback response status=%d body=%s", fallbackRecorder.Code, fallbackRecorder.Body.String())
	}

	unauthorized := httptest.NewRequest(http.MethodGet, "/backend-api/models", nil)
	unauthorized.Header.Set("Authorization", "Bearer wrong-frontend-key")
	unauthorized.Header.Set("ChatGPT-Account-ID", "acct-123")
	unauthorizedRecorder := httptest.NewRecorder()
	server.engine.ServeHTTP(unauthorizedRecorder, unauthorized)
	if unauthorizedRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized response=%d body=%s", unauthorizedRecorder.Code, unauthorizedRecorder.Body.String())
	}

	missing := httptest.NewRequest(http.MethodGet, "/backend-api/models", nil)
	missing.Header.Set("Authorization", "Bearer frontend-key")
	missingRecorder := httptest.NewRecorder()
	server.engine.ServeHTTP(missingRecorder, missing)
	if missingRecorder.Code != http.StatusNotFound || upstreamCalls.Load() != 2 {
		t.Fatalf("ineligible response=%d upstream calls=%d", missingRecorder.Code, upstreamCalls.Load())
	}
}

func copyFile(t *testing.T, target, source string) {
	t.Helper()
	input, errOpen := os.Open(source)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer input.Close()
	output, errCreate := os.Create(target)
	if errCreate != nil {
		t.Fatal(errCreate)
	}
	if _, errCopy := io.Copy(output, input); errCopy != nil {
		_ = output.Close()
		t.Fatal(errCopy)
	}
	if errClose := output.Close(); errClose != nil {
		t.Fatal(errClose)
	}
}
