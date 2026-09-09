package pluginhost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

const (
	maxIngressRoutes          = 16
	maxIngressMethods         = 16
	maxIngressRequiredHeaders = 16
	maxIngressOrigins         = 8
	maxCredentialSources      = 16
	maxStoredJWTBytes         = 64 << 10
)

var ingressReservedPrefixes = []string{
	"/v0/management",
	"/v0/resource/plugins",
}

var ingressMethodsAllowed = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodConnect: {},
	http.MethodOptions: {}, http.MethodTrace: {},
}

var ingressIdentityPaths = map[string]struct{}{
	"account_id": {}, "chatgpt_account_id": {}, "email": {}, "organization_id": {},
	"tokens.account_id": {}, "tokens.chatgpt_account_id": {}, "tokens.email": {}, "tokens.organization_id": {},
}

var ingressJWTPaths = map[string]struct{}{"id_token": {}, "tokens.id_token": {}}

var ingressSecretPaths = map[string]struct{}{
	"access_token": {}, "auth_token": {}, "bearer_token": {},
	"tokens.access_token": {}, "tokens.auth_token": {}, "tokens.bearer_token": {},
}

type ingressRouteRecord struct {
	pluginID string
	path     string
	version  string
	route    pluginapi.IngressRoute
	handler  pluginapi.IngressProxy
	methods  map[string]struct{}
	origins  map[string]struct{}
}

// RegisterIngressRoutes rebuilds the authenticated fallback ingress table.
// Concrete Gin routes take precedence because ingress is dispatched only from NoRoute.
func (h *Host) RegisterIngressRoutes(ctx context.Context) {
	if h == nil {
		return
	}
	next := make([]ingressRouteRecord, 0)
	for _, record := range h.activeRecords() {
		handler := record.plugin.Capabilities.IngressProxy
		if handler == nil || h.isPluginFused(record.id) {
			continue
		}
		resp, errRegister := h.callIngressRegistrar(ctx, record, handler)
		if errRegister != nil {
			log.WithError(errRegister).WithField("plugin_id", record.id).Warn("pluginhost: ingress registrar failed")
			continue
		}
		if len(resp.Routes) > maxIngressRoutes {
			log.WithField("plugin_id", record.id).Warn("pluginhost: ingress route limit exceeded")
			continue
		}
		for _, declared := range resp.Routes {
			normalized, errNormalize := normalizeIngressRoute(declared)
			if errNormalize != nil {
				log.WithError(errNormalize).WithField("plugin_id", record.id).Warn("pluginhost: invalid ingress route skipped")
				continue
			}
			conflict := false
			for _, existing := range next {
				if ingressRoutesOverlap(existing, normalized) {
					conflict = true
					break
				}
			}
			if conflict {
				log.WithFields(log.Fields{"plugin_id": record.id, "path_prefix": normalized.route.PathPrefix}).Warn("pluginhost: ingress route conflicts with a higher-priority plugin and was skipped")
				continue
			}
			normalized.pluginID = record.id
			normalized.path = record.path
			normalized.version = record.version
			normalized.handler = handler
			next = append(next, normalized)
		}
	}
	h.mu.Lock()
	h.ingressRoutes = next
	h.mu.Unlock()
}

func (h *Host) callIngressRegistrar(ctx context.Context, record capabilityRecord, handler pluginapi.IngressProxy) (resp pluginapi.IngressRegistrationResponse, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "IngressProxy.RegisterIngress", recovered)
			resp = pluginapi.IngressRegistrationResponse{}
			err = fmt.Errorf("ingress registrar panic: %v", recovered)
		}
	}()
	return handler.RegisterIngress(ctx, pluginapi.IngressRegistrationRequest{Plugin: record.meta})
}

func normalizeIngressRoute(route pluginapi.IngressRoute) (ingressRouteRecord, error) {
	prefix := strings.TrimSpace(route.PathPrefix)
	if prefix == "/" || !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") || strings.ContainsAny(prefix, " \t\r\n:*") || strings.Contains(prefix, "..") {
		return ingressRouteRecord{}, fmt.Errorf("unsafe path prefix %q", route.PathPrefix)
	}
	for _, reserved := range ingressReservedPrefixes {
		if strings.HasPrefix(prefix, reserved) || strings.HasPrefix(reserved, prefix) {
			return ingressRouteRecord{}, fmt.Errorf("path prefix %q overlaps reserved prefix %q", prefix, reserved)
		}
	}
	if len(route.Methods) == 0 || len(route.Methods) > maxIngressMethods {
		return ingressRouteRecord{}, fmt.Errorf("methods must contain 1-%d entries", maxIngressMethods)
	}
	methods := make(map[string]struct{}, len(route.Methods))
	for _, rawMethod := range route.Methods {
		method := strings.ToUpper(strings.TrimSpace(rawMethod))
		if _, allowed := ingressMethodsAllowed[method]; !allowed {
			return ingressRouteRecord{}, fmt.Errorf("method %q is not allowed", rawMethod)
		}
		methods[method] = struct{}{}
	}
	if len(route.RequiredHeaders) > maxIngressRequiredHeaders {
		return ingressRouteRecord{}, fmt.Errorf("required header limit exceeded")
	}
	requiredHeaders := make([]string, 0, len(route.RequiredHeaders))
	for _, rawHeader := range route.RequiredHeaders {
		header := http.CanonicalHeaderKey(strings.TrimSpace(rawHeader))
		if header == "" || strings.ContainsAny(header, " \t\r\n:") {
			return ingressRouteRecord{}, fmt.Errorf("invalid required header %q", rawHeader)
		}
		requiredHeaders = append(requiredHeaders, header)
	}
	if len(route.UpstreamOrigins) == 0 || len(route.UpstreamOrigins) > maxIngressOrigins {
		return ingressRouteRecord{}, fmt.Errorf("upstream origins must contain 1-%d entries", maxIngressOrigins)
	}
	origins := make(map[string]struct{}, len(route.UpstreamOrigins))
	for _, rawOrigin := range route.UpstreamOrigins {
		origin, errOrigin := canonicalIngressOrigin(rawOrigin)
		if errOrigin != nil {
			return ingressRouteRecord{}, errOrigin
		}
		origins[origin] = struct{}{}
	}
	route.PathPrefix = prefix
	route.RequiredHeaders = requiredHeaders
	return ingressRouteRecord{route: route, methods: methods, origins: origins}, nil
}

func canonicalIngressOrigin(raw string) (string, error) {
	parsed, errParse := url.Parse(strings.TrimSpace(raw))
	if errParse != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("unsafe upstream origin %q", raw)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("upstream origin %q contains a path", raw)
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func ingressRoutesOverlap(left, right ingressRouteRecord) bool {
	if !strings.HasPrefix(left.route.PathPrefix, right.route.PathPrefix) && !strings.HasPrefix(right.route.PathPrefix, left.route.PathPrefix) {
		return false
	}
	for method := range left.methods {
		if _, ok := right.methods[method]; ok {
			return true
		}
	}
	return false
}

// IngressEligible reports whether a request matches a declared route and its cheap eligibility requirements.
// Authentication must run after this check and before ServeIngressHTTP.
func (h *Host) IngressEligible(r *http.Request) bool {
	_, ok := h.ingressRouteForRequest(r)
	return ok
}

func (h *Host) ingressRouteForRequest(r *http.Request) (ingressRouteRecord, bool) {
	if h == nil || r == nil || r.URL == nil {
		return ingressRouteRecord{}, false
	}
	h.mu.Lock()
	routes := append([]ingressRouteRecord(nil), h.ingressRoutes...)
	h.mu.Unlock()
	for _, route := range routes {
		if _, ok := route.methods[strings.ToUpper(r.Method)]; !ok || !strings.HasPrefix(r.URL.Path, route.route.PathPrefix) {
			continue
		}
		eligible := true
		for _, header := range route.route.RequiredHeaders {
			if strings.TrimSpace(r.Header.Get(header)) == "" {
				eligible = false
				break
			}
		}
		if eligible && !h.isPluginFused(route.pluginID) && h.pluginIdentityCurrent(route.pluginID, route.path, route.version) {
			return route, true
		}
	}
	return ingressRouteRecord{}, false
}

// ServeIngressHTTP asks the matching plugin for a data-only plan and streams it under host policy.
// The caller must authenticate the frontend request first.
func (h *Host) ServeIngressHTTP(w http.ResponseWriter, r *http.Request) bool {
	route, ok := h.ingressRouteForRequest(r)
	if !ok || w == nil {
		return false
	}
	request := pluginapi.IngressRequest{
		Method:         r.Method,
		Path:           r.URL.Path,
		EscapedPath:    r.URL.EscapedPath(),
		RawQuery:       r.URL.RawQuery,
		Headers:        sanitizedIngressHeaders(r.Header),
		PresentHeaders: ingressHeaderNames(r.Header),
	}
	resp, errHandle := h.callIngressHandler(r.Context(), route, request)
	if errHandle != nil {
		log.WithError(errHandle).WithFields(log.Fields{"plugin_id": route.pluginID, "method": r.Method, "path": r.URL.Path}).Warn("pluginhost: ingress handler failed")
		writeIngressError(w, http.StatusBadGateway, "ingress plugin failed")
		return true
	}
	if !resp.Handled {
		return false
	}
	if resp.Plan == nil {
		writeIngressError(w, http.StatusBadGateway, "invalid ingress proxy plan")
		return true
	}
	if errProxy := h.executeIngressPlan(w, r, route, *resp.Plan); errProxy != nil {
		log.WithError(errProxy).WithFields(log.Fields{"plugin_id": route.pluginID, "method": r.Method, "path": r.URL.Path}).Warn("pluginhost: ingress proxy failed")
		if !responseStarted(w) {
			writeIngressError(w, http.StatusBadGateway, "upstream request failed")
		}
	}
	return true
}

func (h *Host) callIngressHandler(ctx context.Context, route ingressRouteRecord, req pluginapi.IngressRequest) (resp pluginapi.IngressResponse, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(route.pluginID, "IngressProxy.HandleIngress", recovered)
			resp = pluginapi.IngressResponse{}
			err = fmt.Errorf("ingress handler panic: %v", recovered)
		}
	}()
	return route.handler.HandleIngress(ctx, req)
}

func (h *Host) executeIngressPlan(w http.ResponseWriter, inbound *http.Request, route ingressRouteRecord, plan pluginapi.IngressProxyPlan) error {
	upstreamURL, errURL := url.Parse(plan.UpstreamURL)
	if errURL != nil || upstreamURL == nil || upstreamURL.Host == "" || upstreamURL.User != nil || upstreamURL.Fragment != "" {
		return fmt.Errorf("invalid upstream URL")
	}
	origin, errOrigin := canonicalIngressOrigin(upstreamURL.Scheme + "://" + upstreamURL.Host)
	if errOrigin != nil {
		return errOrigin
	}
	if _, allowed := route.origins[origin]; !allowed {
		return fmt.Errorf("upstream origin is not registered")
	}

	upstreamReq, errRequest := http.NewRequestWithContext(inbound.Context(), inbound.Method, upstreamURL.String(), inbound.Body)
	if errRequest != nil {
		return fmt.Errorf("create upstream request: %w", errRequest)
	}
	upstreamReq.ContentLength = inbound.ContentLength
	copyIngressHeaders(upstreamReq.Header, inbound.Header)

	matchedAuth, authSource, errCredential := h.applyIngressCredential(upstreamReq.Header, plan.Credential)
	if errCredential != nil {
		return errCredential
	}
	client, errTransport := h.ingressHTTPClient(inbound.Context(), strings.TrimSpace(plan.Transport), matchedAuth)
	if errTransport != nil {
		return errTransport
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

	start := time.Now()
	resp, errDo := client.Do(upstreamReq)
	if errDo != nil {
		// Transport errors often include the full URL. Do not log query credentials.
		return fmt.Errorf("execute upstream request failed")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).WithField("plugin_id", route.pluginID).Warn("pluginhost: ingress response close failed")
		}
	}()
	copyIngressHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if errCopy := copyIngressBody(w, resp.Body); errCopy != nil {
		return fmt.Errorf("stream upstream response: %w", errCopy)
	}
	log.WithFields(log.Fields{"plugin_id": route.pluginID, "method": inbound.Method, "path": inbound.URL.Path, "upstream_status": resp.StatusCode, "auth_source": authSource, "duration_ms": time.Since(start).Milliseconds()}).Info("pluginhost: ingress proxy completed")
	return nil
}

func (h *Host) ingressHTTPClient(ctx context.Context, policy string, auth *coreauth.Auth) (*http.Client, error) {
	switch policy {
	case "", "standard":
		return helps.NewProxyAwareHTTPClient(ctx, h.currentRuntimeConfig(), auth, 0), nil
	case "utls":
		return helps.NewUtlsHTTPClient(ctx, h.currentRuntimeConfig(), auth, 0), nil
	default:
		return nil, fmt.Errorf("unsupported ingress transport policy %q", policy)
	}
}

func (h *Host) applyIngressCredential(headers http.Header, use *pluginapi.IngressCredentialUse) (*coreauth.Auth, string, error) {
	if use == nil {
		return nil, "inbound", nil
	}
	if errValidate := validateCredentialUse(*use); errValidate != nil {
		return nil, "", errValidate
	}
	auth := h.selectIngressCredential(use.Selector)
	if auth == nil {
		if use.FallbackToInbound {
			return nil, "inbound", nil
		}
		return nil, "", fmt.Errorf("no matching credential")
	}
	value := firstCredentialValue(auth, use.Injection.ValueSources)
	if value == "" {
		if use.FallbackToInbound {
			return auth, "inbound", nil
		}
		return nil, "", fmt.Errorf("matching credential has no usable secret")
	}
	headers.Set(use.Injection.Header, use.Injection.Prefix+value)
	return auth, "stored", nil
}

func validateCredentialUse(use pluginapi.IngressCredentialUse) error {
	selector := use.Selector
	if strings.TrimSpace(selector.Provider) == "" || strings.TrimSpace(selector.Equals) == "" || selector.Order != "oldest" || len(selector.IdentitySources) == 0 || len(selector.IdentitySources) > maxCredentialSources {
		return fmt.Errorf("invalid credential selector")
	}
	for _, source := range selector.IdentitySources {
		if !validIdentitySource(source) {
			return fmt.Errorf("invalid credential identity source")
		}
	}
	injection := use.Injection
	if !strings.EqualFold(strings.TrimSpace(injection.Header), "Authorization") || strings.ContainsAny(injection.Prefix, "\r\n") || len(injection.ValueSources) == 0 || len(injection.ValueSources) > maxCredentialSources {
		return fmt.Errorf("invalid credential injection")
	}
	for _, source := range injection.ValueSources {
		if source.Kind != "metadata" && source.Kind != "attribute" {
			return fmt.Errorf("invalid credential secret source")
		}
		if _, ok := ingressSecretPaths[source.Path]; !ok || source.Claim != "" {
			return fmt.Errorf("invalid credential secret path")
		}
	}
	return nil
}

func validIdentitySource(source pluginapi.CredentialValueSource) bool {
	switch source.Kind {
	case "attribute", "metadata":
		_, ok := ingressIdentityPaths[source.Path]
		return ok && source.Claim == ""
	case "jwt_claim":
		_, ok := ingressJWTPaths[source.Path]
		return ok && validJSONPointer(source.Claim)
	default:
		return false
	}
}

func (h *Host) selectIngressCredential(selector pluginapi.CredentialSelector) *coreauth.Auth {
	manager := h.currentAuthManager()
	if manager == nil {
		return nil
	}
	auths := manager.List()
	sort.SliceStable(auths, func(i, j int) bool {
		if auths[i] == nil || auths[j] == nil {
			return auths[j] != nil
		}
		if !auths[i].CreatedAt.Equal(auths[j].CreatedAt) {
			return auths[i].CreatedAt.Before(auths[j].CreatedAt)
		}
		return auths[i].ID < auths[j].ID
	})
	for _, candidate := range auths {
		if !activeIngressCredential(candidate, selector.Provider) {
			continue
		}
		identity := firstCredentialValue(candidate, selector.IdentitySources)
		matched := identity == selector.Equals
		if selector.CaseInsensitive {
			matched = strings.EqualFold(identity, selector.Equals)
		}
		if identity != "" && matched {
			return candidate
		}
	}
	return nil
}

func activeIngressCredential(auth *coreauth.Auth, provider string) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), strings.TrimSpace(provider)) && !auth.Disabled && !auth.Unavailable && (auth.Status == "" || auth.Status == coreauth.StatusActive)
}

func firstCredentialValue(auth *coreauth.Auth, sources []pluginapi.CredentialValueSource) string {
	for _, source := range sources {
		var value string
		switch source.Kind {
		case "attribute":
			value = strings.TrimSpace(auth.Attributes[source.Path])
		case "metadata":
			value = metadataString(auth.Metadata, source.Path)
		case "jwt_claim":
			value = storedJWTClaim(metadataString(auth.Metadata, source.Path), source.Claim)
		}
		if value != "" {
			return value
		}
	}
	return ""
}

func metadataString(metadata map[string]any, path string) string {
	parts := strings.Split(path, ".")
	var current any = metadata
	for _, part := range parts {
		switch values := current.(type) {
		case map[string]any:
			current = values[part]
		case map[string]string:
			current = values[part]
		default:
			return ""
		}
	}
	switch value := current.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func storedJWTClaim(token, pointer string) string {
	if token == "" || len(token) > maxStoredJWTBytes*2 {
		return ""
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil || len(payload) > maxStoredJWTBytes {
		return ""
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var claims any
	if errJSON := decoder.Decode(&claims); errJSON != nil {
		return ""
	}
	current := claims
	for _, encoded := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		key := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		values, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = values[key]
	}
	value, _ := current.(string)
	return strings.TrimSpace(value)
}

func validJSONPointer(pointer string) bool {
	if len(pointer) < 2 || len(pointer) > 512 || pointer[0] != '/' || strings.ContainsAny(pointer, "\r\n") {
		return false
	}
	parts := strings.Split(pointer[1:], "/")
	return len(parts) > 0 && len(parts) <= 8
}

func sanitizedIngressHeaders(headers http.Header) http.Header {
	out := cloneHeader(headers)
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie"} {
		out.Del(name)
	}
	return out
}

func ingressHeaderNames(headers http.Header) []string {
	out := make([]string, 0, len(headers))
	for name := range headers {
		out = append(out, http.CanonicalHeaderKey(name))
	}
	sort.Strings(out)
	return out
}

var hopByHopHeaders = map[string]struct{}{
	"connection": {}, "proxy-authenticate": {}, "proxy-authorization": {}, "proxy-connection": {},
	"te": {}, "trailer": {}, "transfer-encoding": {}, "upgrade": {},
}

func copyIngressHeaders(dst, src http.Header) {
	skip := make(map[string]struct{}, len(hopByHopHeaders)+4)
	for name := range hopByHopHeaders {
		skip[name] = struct{}{}
	}
	for _, value := range src.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			skip[strings.ToLower(strings.TrimSpace(token))] = struct{}{}
		}
	}
	for name, values := range src {
		if _, omitted := skip[strings.ToLower(name)]; omitted {
			continue
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func copyIngressBody(dst http.ResponseWriter, src io.Reader) error {
	buffer := make([]byte, 32*1024)
	flusher, canFlush := dst.(http.Flusher)
	for {
		n, errRead := src.Read(buffer)
		if n > 0 {
			if _, errWrite := dst.Write(buffer[:n]); errWrite != nil {
				return errWrite
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if errRead == io.EOF {
			return nil
		}
		if errRead != nil {
			return errRead
		}
	}
}

type responseState interface{ Written() bool }

func responseStarted(w http.ResponseWriter) bool {
	state, ok := w.(responseState)
	return ok && state.Written()
}

func writeIngressError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, fmt.Sprintf(`{"error":%q}`, message))
}
