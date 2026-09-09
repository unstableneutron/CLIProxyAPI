package pluginhost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

type testIngressProxy struct {
	register func(context.Context, pluginapi.IngressRegistrationRequest) (pluginapi.IngressRegistrationResponse, error)
	handle   func(context.Context, pluginapi.IngressRequest) (pluginapi.IngressResponse, error)
}

func (p testIngressProxy) RegisterIngress(ctx context.Context, req pluginapi.IngressRegistrationRequest) (pluginapi.IngressRegistrationResponse, error) {
	return p.register(ctx, req)
}

func (p testIngressProxy) HandleIngress(ctx context.Context, req pluginapi.IngressRequest) (pluginapi.IngressResponse, error) {
	return p.handle(ctx, req)
}

func newIngressTestHost(proxy pluginapi.IngressProxy) *Host {
	host := newHostWithRecords(capabilityRecord{
		id: "ingress-test", path: "/tmp/ingress-test.so", version: "1",
		meta:   pluginapi.Metadata{Name: "ingress-test", Version: "1"},
		plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{IngressProxy: proxy}},
	})
	host.RegisterIngressRoutes(context.Background())
	return host
}

func ingressTestRoute(origin string) pluginapi.IngressRegistrationResponse {
	return pluginapi.IngressRegistrationResponse{Routes: []pluginapi.IngressRoute{{
		Methods: []string{http.MethodPatch, http.MethodPost, http.MethodGet}, PathPrefix: "/backend-api/",
		RequiredHeaders: []string{"Authorization", "ChatGPT-Account-ID"}, UpstreamOrigins: []string{origin},
	}}}
}

func TestIngressProxyPreservesRequestAndResponse(t *testing.T) {
	var bodyReads atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.EscapedPath(), "/backend-api/files/a%2Fb"; got != want {
			t.Errorf("escaped path = %q, want %q", got, want)
		}
		if got, want := r.URL.RawQuery, "x=a%2Bb&x=a+b"; got != want {
			t.Errorf("raw query = %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer inbound-secret" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Proxy-Authorization"); got != "" {
			t.Errorf("Proxy-Authorization = %q, want empty", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "streamed-upload" {
			t.Errorf("body = %q", body)
		}
		w.Header().Set("X-Upstream", "ok")
		w.Header().Set("Connection", "X-Private")
		w.Header().Set("X-Private", "drop")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "streamed-download")
	}))
	defer upstream.Close()

	proxy := testIngressProxy{
		register: func(context.Context, pluginapi.IngressRegistrationRequest) (pluginapi.IngressRegistrationResponse, error) {
			return ingressTestRoute(upstream.URL), nil
		},
		handle: func(_ context.Context, req pluginapi.IngressRequest) (pluginapi.IngressResponse, error) {
			if bodyReads.Load() != 0 {
				t.Fatal("request body was read before the plugin returned its plan")
			}
			if req.Headers.Get("Authorization") != "" || !containsString(req.PresentHeaders, "Authorization") {
				t.Fatalf("sensitive header was not presence-only: %#v %#v", req.Headers, req.PresentHeaders)
			}
			return pluginapi.IngressResponse{Handled: true, Plan: &pluginapi.IngressProxyPlan{
				UpstreamURL: upstream.URL + req.EscapedPath + "?" + req.RawQuery,
				Transport:   "standard",
			}}, nil
		},
	}
	host := newIngressTestHost(proxy)
	body := &countingReader{reader: strings.NewReader("streamed-upload"), reads: &bodyReads}
	req := httptest.NewRequest(http.MethodPatch, "/backend-api/files/a%2Fb?x=a%2Bb&x=a+b", body)
	req.Header.Set("Authorization", "Bearer inbound-secret")
	req.Header.Set("ChatGPT-Account-ID", "acct")
	req.Header.Set("Proxy-Authorization", "proxy-secret")
	recorder := httptest.NewRecorder()

	if !host.IngressEligible(req) || !host.ServeIngressHTTP(recorder, req) {
		t.Fatal("request was not served")
	}
	if recorder.Code != http.StatusAccepted || recorder.Body.String() != "streamed-download" {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Upstream") != "ok" || recorder.Header().Get("Connection") != "" || recorder.Header().Get("X-Private") != "" {
		t.Fatalf("response headers = %#v", recorder.Header())
	}
}

func TestIngressCredentialSelectionUsesOldestSharedPoolMatch(t *testing.T) {
	selector := pluginapi.CredentialSelector{
		Provider: "codex", Equals: "ACCT-123", CaseInsensitive: true, Order: "oldest",
		IdentitySources: []pluginapi.CredentialValueSource{{Kind: "attribute", Path: "account_id"}, {Kind: "metadata", Path: "account_id"}, {Kind: "jwt_claim", Path: "id_token", Claim: "/https:~1~1api.openai.com~1auth/chatgpt_account_id"}},
	}
	host := New()
	manager := coreauth.NewManager(nil, nil, nil)
	host.SetAuthManager(manager)
	registerIngressAuth(t, manager, &coreauth.Auth{ID: "wrong-first-value", Provider: "codex", Status: coreauth.StatusActive, CreatedAt: time.Unix(1, 0), Attributes: map[string]string{"account_id": "other"}, Metadata: map[string]any{"account_id": "acct-123", "access_token": "wrong"}})
	registerIngressAuth(t, manager, &coreauth.Auth{ID: "newer", Provider: "codex", Status: coreauth.StatusActive, CreatedAt: time.Unix(3, 0), Metadata: map[string]any{"account_id": "acct-123", "access_token": "new"}})
	registerIngressAuth(t, manager, &coreauth.Auth{ID: "older-jwt", Provider: "codex", Status: coreauth.StatusActive, CreatedAt: time.Unix(2, 0), Metadata: map[string]any{"id_token": testAccountJWT(t, "acct-123"), "access_token": "old"}})

	headers := http.Header{"Authorization": {"Bearer inbound"}}
	auth, source, errApply := host.applyIngressCredential(headers, &pluginapi.IngressCredentialUse{
		Selector:          selector,
		Injection:         pluginapi.CredentialInjection{Header: "Authorization", Prefix: "Bearer ", ValueSources: []pluginapi.CredentialValueSource{{Kind: "metadata", Path: "access_token"}}},
		FallbackToInbound: true,
	})
	if errApply != nil {
		t.Fatal(errApply)
	}
	if auth == nil || auth.ID != "older-jwt" || source != "stored" || headers.Get("Authorization") != "Bearer old" {
		t.Fatalf("selection = auth %#v source %q header %q", auth, source, headers.Get("Authorization"))
	}
}

func TestIngressCredentialFallbackRetainsInboundAuthorization(t *testing.T) {
	host := New()
	headers := http.Header{"Authorization": {"Bearer inbound"}}
	auth, source, errApply := host.applyIngressCredential(headers, &pluginapi.IngressCredentialUse{
		Selector:          pluginapi.CredentialSelector{Provider: "codex", Equals: "missing", Order: "oldest", IdentitySources: []pluginapi.CredentialValueSource{{Kind: "metadata", Path: "account_id"}}},
		Injection:         pluginapi.CredentialInjection{Header: "Authorization", Prefix: "Bearer ", ValueSources: []pluginapi.CredentialValueSource{{Kind: "metadata", Path: "access_token"}}},
		FallbackToInbound: true,
	})
	if errApply != nil || auth != nil || source != "inbound" || headers.Get("Authorization") != "Bearer inbound" {
		t.Fatalf("fallback = auth %#v source %q header %q err %v", auth, source, headers.Get("Authorization"), errApply)
	}
}

func TestIngressRejectsRouteOverlapReservedOriginsAndSecretPaths(t *testing.T) {
	if _, errRoute := normalizeIngressRoute(pluginapi.IngressRoute{Methods: []string{"GET"}, PathPrefix: "/v0/", UpstreamOrigins: []string{"https://example.test"}}); errRoute == nil {
		t.Fatal("reserved route accepted")
	}
	if _, errRoute := normalizeIngressRoute(pluginapi.IngressRoute{Methods: []string{"GET"}, PathPrefix: "/safe/", UpstreamOrigins: []string{"file:///tmp/data"}}); errRoute == nil {
		t.Fatal("unsafe origin accepted")
	}
	errUse := validateCredentialUse(pluginapi.IngressCredentialUse{
		Selector:  pluginapi.CredentialSelector{Provider: "codex", Equals: "acct", Order: "oldest", IdentitySources: []pluginapi.CredentialValueSource{{Kind: "metadata", Path: "account_id"}}},
		Injection: pluginapi.CredentialInjection{Header: "Authorization", ValueSources: []pluginapi.CredentialValueSource{{Kind: "metadata", Path: "refresh_token"}}},
	})
	if errUse == nil {
		t.Fatal("unallowlisted secret path accepted")
	}
	left, _ := normalizeIngressRoute(pluginapi.IngressRoute{Methods: []string{"GET"}, PathPrefix: "/one/", UpstreamOrigins: []string{"https://example.test"}})
	right, _ := normalizeIngressRoute(pluginapi.IngressRoute{Methods: []string{"GET"}, PathPrefix: "/one/nested/", UpstreamOrigins: []string{"https://example.test"}})
	if !ingressRoutesOverlap(left, right) {
		t.Fatal("overlapping prefixes and methods were not detected")
	}
}

func TestIngressCancellationReachesUpstream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	host := newIngressTestHost(testIngressProxy{
		register: func(context.Context, pluginapi.IngressRegistrationRequest) (pluginapi.IngressRegistrationResponse, error) {
			return ingressTestRoute(upstream.URL), nil
		},
		handle: func(_ context.Context, req pluginapi.IngressRequest) (pluginapi.IngressResponse, error) {
			return pluginapi.IngressResponse{Handled: true, Plan: &pluginapi.IngressProxyPlan{UpstreamURL: upstream.URL + req.Path, Transport: "standard"}}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/backend-api/models", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("ChatGPT-Account-ID", "acct")
	done := make(chan struct{})
	go func() {
		host.ServeIngressHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not observe cancellation")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop after cancellation")
	}
}

func TestIngressDownstreamDisconnectCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 64*1024))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	host := newIngressTestHost(testIngressProxy{
		register: func(context.Context, pluginapi.IngressRegistrationRequest) (pluginapi.IngressRegistrationResponse, error) {
			return ingressTestRoute(upstream.URL), nil
		},
		handle: func(_ context.Context, req pluginapi.IngressRequest) (pluginapi.IngressResponse, error) {
			return pluginapi.IngressResponse{Handled: true, Plan: &pluginapi.IngressProxyPlan{UpstreamURL: upstream.URL + req.Path, Transport: "standard"}}, nil
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/backend-api/stream", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("ChatGPT-Account-ID", "acct")
	done := make(chan struct{})
	go func() {
		host.ServeIngressHTTP(&disconnectWriter{header: make(http.Header)}, req)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not start")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not stop after downstream disconnect")
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream was not canceled after downstream disconnect")
	}
}

func TestIngressFailureLogsDoNotContainRequestSecrets(t *testing.T) {
	var logs bytes.Buffer
	oldOutput := log.StandardLogger().Out
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })
	host := newIngressTestHost(testIngressProxy{
		register: func(context.Context, pluginapi.IngressRegistrationRequest) (pluginapi.IngressRegistrationResponse, error) {
			return ingressTestRoute("http://127.0.0.1:1"), nil
		},
		handle: func(_ context.Context, req pluginapi.IngressRequest) (pluginapi.IngressResponse, error) {
			return pluginapi.IngressResponse{Handled: true, Plan: &pluginapi.IngressProxyPlan{UpstreamURL: "http://127.0.0.1:1/fail?token=query-secret", Transport: "standard"}}, nil
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/backend-api/fail?token=inbound-secret", nil)
	req.Header.Set("Authorization", "Bearer header-secret")
	req.Header.Set("ChatGPT-Account-ID", "account-secret")
	recorder := httptest.NewRecorder()
	host.ServeIngressHTTP(recorder, req)
	if recorder.Code != http.StatusBadGateway || !json.Valid(recorder.Body.Bytes()) {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	for _, secret := range []string{"query-secret", "inbound-secret", "header-secret", "account-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs contain secret %q: %s", secret, logs.String())
		}
	}
}

type countingReader struct {
	reader io.Reader
	reads  *atomic.Int64
}

type disconnectWriter struct {
	header http.Header
}

func (w *disconnectWriter) Header() http.Header { return w.header }
func (w *disconnectWriter) WriteHeader(_ int)   {}
func (w *disconnectWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("downstream disconnected")
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return r.reader.Read(p)
}

func registerIngressAuth(t *testing.T, manager *coreauth.Manager, auth *coreauth.Auth) {
	t.Helper()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
}

func testAccountJWT(t *testing.T, accountID string) string {
	t.Helper()
	payload, errMarshal := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID}})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
