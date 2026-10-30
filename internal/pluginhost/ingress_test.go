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

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
	return newIngressTestHostWithSchema(proxy, 0)
}

func newIngressTestHostWithSchema(proxy pluginapi.IngressProxy, schemaVersion uint32) *Host {
	host := newHostWithRecords(capabilityRecord{
		id: "ingress-test", path: "/tmp/ingress-test.so", version: "1",
		meta:   pluginapi.Metadata{Name: "ingress-test", Version: "1"},
		plugin: pluginapi.Plugin{SchemaVersion: schemaVersion, Capabilities: pluginapi.Capabilities{IngressProxy: proxy}},
	})
	host.RegisterIngressRoutes(context.Background())
	return host
}

func TestIngressHandlerRouteBuffersBoundedBodyAndWritesDirectResponse(t *testing.T) {
	var calls atomic.Int64
	// HTML-significant characters must survive: handler responses are never sanitized.
	responseBody := []byte(`{"data":[{"embedding":[0.1,-2e-7]}],"note":"<b>&'"}`)
	proxy := testIngressProxy{
		register: func(context.Context, pluginapi.IngressRegistrationRequest) (pluginapi.IngressRegistrationResponse, error) {
			return pluginapi.IngressRegistrationResponse{Routes: []pluginapi.IngressRoute{{
				Methods: []string{http.MethodPost}, Path: "/v1/embeddings", MaxBodyBytes: 16,
			}}}, nil
		},
		handle: func(_ context.Context, req pluginapi.IngressRequest) (pluginapi.IngressResponse, error) {
			calls.Add(1)
			if string(req.Body) != `{"model":"m"}` {
				t.Errorf("body = %q", req.Body)
			}
			if req.Headers.Get("Authorization") != "" || !containsString(req.PresentHeaders, "Authorization") {
				t.Errorf("authorization was exposed: %#v", req.Headers)
			}
			return pluginapi.IngressResponse{Handled: true, Response: &pluginapi.IngressDirectResponse{
				StatusCode: http.StatusCreated,
				Headers:    http.Header{"Content-Type": {"application/json"}, "Connection": {"close"}, "Content-Length": {"1"}},
				Body:       responseBody,
			}}, nil
		},
	}
	host := newIngressTestHostWithSchema(proxy, 9)

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	recorder := httptest.NewRecorder()
	if !host.IngressEligible(req) || !host.ServeIngressHTTP(recorder, req) {
		t.Fatal("handler route was not served")
	}
	if recorder.Code != http.StatusCreated || !bytes.Equal(recorder.Body.Bytes(), responseBody) {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Connection") != "" || recorder.Header().Get("Content-Length") != "" || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response headers = %#v", recorder.Header())
	}

	for _, path := range []string{"/v1/embeddings/", "/v1/embeddings/extra", "/v1/embedding"} {
		if host.IngressEligible(httptest.NewRequest(http.MethodPost, path, nil)) {
			t.Fatalf("exact handler route matched %q", path)
		}
	}
	if host.IngressEligible(httptest.NewRequest(http.MethodGet, "/v1/embeddings", nil)) {
		t.Fatal("handler route matched an undeclared method")
	}

	for name, body := range map[string]io.Reader{
		"declared length": strings.NewReader(`{"model":"too-large"}`),
		"unknown length":  io.MultiReader(strings.NewReader(`{"model":`), strings.NewReader(`"too-large"}`)),
	} {
		oversized := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
		if name == "unknown length" {
			oversized.ContentLength = -1
		}
		recorder = httptest.NewRecorder()
		host.ServeIngressHTTP(recorder, oversized)
		if recorder.Code != http.StatusRequestEntityTooLarge || !json.Valid(recorder.Body.Bytes()) {
			t.Fatalf("%s: oversized response = %d %q", name, recorder.Code, recorder.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("plugin calls = %d, want 1 (oversized bodies must not reach the plugin)", calls.Load())
	}
}

func TestIngressHandlerRouteRequiresDirectResponse(t *testing.T) {
	for name, resp := range map[string]pluginapi.IngressResponse{
		"missing response": {Handled: true},
		"proxy plan":       {Handled: true, Plan: &pluginapi.IngressProxyPlan{UpstreamURL: "https://example.test"}, Response: &pluginapi.IngressDirectResponse{}},
		"invalid status":   {Handled: true, Response: &pluginapi.IngressDirectResponse{StatusCode: 101}},
	} {
		t.Run(name, func(t *testing.T) {
			host := newIngressTestHostWithSchema(testIngressProxy{
				register: func(context.Context, pluginapi.IngressRegistrationRequest) (pluginapi.IngressRegistrationResponse, error) {
					return pluginapi.IngressRegistrationResponse{Routes: []pluginapi.IngressRoute{{Methods: []string{http.MethodPost}, Path: "/v1/rerank", MaxBodyBytes: 1024}}}, nil
				},
				handle: func(context.Context, pluginapi.IngressRequest) (pluginapi.IngressResponse, error) { return resp, nil },
			}, 9)
			recorder := httptest.NewRecorder()
			if !host.ServeIngressHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader("{}"))) {
				t.Fatal("request was not served")
			}
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", recorder.Code)
			}
		})
	}
}

func TestIngressHandlerRouteRegistrationPolicy(t *testing.T) {
	handler := func(path string) pluginapi.IngressRoute {
		return pluginapi.IngressRoute{Methods: []string{http.MethodPost}, Path: path, MaxBodyBytes: 1024}
	}
	for name, route := range map[string]pluginapi.IngressRoute{
		"reserved management path":  handler("/v0/management/embeddings"),
		"reserved resource path":    handler("/v0/resource/plugins/x/embeddings"),
		"trailing slash":            handler("/v1/embeddings/"),
		"traversal":                 handler("/v1/../v0/management"),
		"missing body limit":        {Methods: []string{http.MethodPost}, Path: "/v1/embeddings"},
		"body limit above host cap": {Methods: []string{http.MethodPost}, Path: "/v1/embeddings", MaxBodyBytes: maxIngressBodyBytes + 1},
		"upstream origins":          {Methods: []string{http.MethodPost}, Path: "/v1/embeddings", MaxBodyBytes: 1024, UpstreamOrigins: []string{"https://example.test"}},
		"mixed prefix":              {Methods: []string{http.MethodPost}, Path: "/v1/embeddings", PathPrefix: "/v1/", MaxBodyBytes: 1024},
	} {
		if _, errRoute := normalizeIngressRoute(route); errRoute == nil {
			t.Errorf("%s: route accepted", name)
		}
	}

	exact, errExact := normalizeIngressRoute(handler("/v1/embeddings"))
	prefix, errPrefix := normalizeIngressRoute(pluginapi.IngressRoute{Methods: []string{http.MethodPost}, PathPrefix: "/v1/", UpstreamOrigins: []string{"https://example.test"}})
	other, errOther := normalizeIngressRoute(handler("/v1/rerank"))
	if errExact != nil || errPrefix != nil || errOther != nil {
		t.Fatal(errors.Join(errExact, errPrefix, errOther))
	}
	if !ingressRoutesOverlap(exact, prefix) || !ingressRoutesOverlap(prefix, exact) || !ingressRoutesOverlap(exact, exact) {
		t.Fatal("overlapping exact and prefix routes were not detected")
	}
	if ingressRoutesOverlap(exact, other) {
		t.Fatal("distinct exact routes reported as overlapping")
	}

	register := func(context.Context, pluginapi.IngressRegistrationRequest) (pluginapi.IngressRegistrationResponse, error) {
		return pluginapi.IngressRegistrationResponse{Routes: []pluginapi.IngressRoute{handler("/v1/embeddings")}}, nil
	}
	for schema, want := range map[uint32]bool{8: false, 9: true} {
		host := newIngressTestHostWithSchema(testIngressProxy{register: register}, schema)
		if got := host.IngressEligible(httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil)); got != want {
			t.Errorf("schema %d handler route registered = %v, want %v", schema, got, want)
		}
	}
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
			if req.Headers.Get("Authorization") != "" || req.Headers.Get("X-Api-Key") != "" || req.Headers.Get("X-Goog-Api-Key") != "" || !containsString(req.PresentHeaders, "Authorization") || !containsString(req.PresentHeaders, "X-Api-Key") || !containsString(req.PresentHeaders, "X-Goog-Api-Key") {
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
	req.Header.Set("X-Api-Key", "anthropic-frontend-secret")
	req.Header.Set("X-Goog-Api-Key", "google-frontend-secret")
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

func TestIngressPartialResponseDoesNotAppendErrorJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "partial")
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
	req := httptest.NewRequest(http.MethodGet, "/backend-api/partial", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("ChatGPT-Account-ID", "acct")
	recorder := httptest.NewRecorder()
	host.ServeIngressHTTP(recorder, req)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "partial" {
		t.Fatalf("partial response = %d %q, want 200 partial", recorder.Code, recorder.Body.String())
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
