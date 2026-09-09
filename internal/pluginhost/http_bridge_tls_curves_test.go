package pluginhost

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestHostHTTPClientWireProfile_TLSCurvesReachClientHelloAndStayPrivate(t *testing.T) {
	t.Parallel()

	clientHellos := make(chan []tls.CurveID, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.TLS = &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			clientHellos <- append([]tls.CurveID(nil), hello.SupportedCurves...)
			return nil, nil
		},
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	baseTransport := server.Client().Transport.(*http.Transport).Clone()
	originalTLSConfig := baseTransport.TLSClientConfig
	originalCurves := append([]tls.CurveID(nil), originalTLSConfig.CurvePreferences...)
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", baseTransport)
	client := New().newHTTPClient(nil)

	tests := []struct {
		names []string
		want  []tls.CurveID
	}{
		{names: []string{"P-384", "X25519"}, want: []tls.CurveID{tls.X25519, tls.CurveP384}},
		{names: []string{"P-521", "P-256"}, want: []tls.CurveID{tls.CurveP256, tls.CurveP521}},
	}
	for _, test := range tests {
		response, errDo := client.Do(ctx, pluginapi.HTTPRequest{
			URL: server.URL,
			WireProfile: &pluginapi.HTTPWireProfile{
				TLSCurves: test.names,
			},
		})
		if errDo != nil {
			t.Fatalf("Do with curves %v: %v", test.names, errDo)
		}
		if string(response.Body) != "ok" {
			t.Fatalf("body = %q, want ok", response.Body)
		}
		select {
		case got := <-clientHellos:
			if !slices.Equal(got, test.want) {
				t.Fatalf("ClientHello curves = %v, want %v", got, test.want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timeout waiting for ClientHello")
		}
	}

	if baseTransport.TLSClientConfig != originalTLSConfig {
		t.Fatal("base transport TLS config pointer changed")
	}
	if !slices.Equal(baseTransport.TLSClientConfig.CurvePreferences, originalCurves) {
		t.Fatalf("base transport curves = %v, want unchanged %v", baseTransport.TLSClientConfig.CurvePreferences, originalCurves)
	}
}

func TestHostHTTPClientWireProfile_TLSCurvesPreserveHTTP2AndALPN(t *testing.T) {
	t.Parallel()

	baseTransport := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			NextProtos: []string{"h2", "http/1.1"},
		},
	}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", baseTransport)
	httpRequest, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	client := New().newHTTPClient(nil).(*hostHTTPClient)
	httpClient, cleanup, errClient := client.newHTTPClientForRequest(ctx, nil, pluginapi.HTTPRequest{
		URL:         "https://example.com",
		WireProfile: &pluginapi.HTTPWireProfile{TLSCurves: []string{"X25519", "P-256"}},
	}, httpRequest)
	if errClient != nil {
		t.Fatalf("newHTTPClientForRequest: %v", errClient)
	}
	defer cleanup()

	transport := httpClient.Transport.(*http.Transport)
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = false, want preserved true")
	}
	if transport.TLSNextProto != nil {
		t.Fatalf("TLSNextProto = %#v, want preserved nil", transport.TLSNextProto)
	}
	if !slices.Equal(transport.TLSClientConfig.NextProtos, []string{"h2", "http/1.1"}) {
		t.Fatalf("ALPN = %v, want [h2 http/1.1]", transport.TLSClientConfig.NextProtos)
	}
	if !slices.Equal(transport.TLSClientConfig.CurvePreferences, []tls.CurveID{tls.X25519, tls.CurveP256}) {
		t.Fatalf("curves = %v", transport.TLSClientConfig.CurvePreferences)
	}
}

func TestHostHTTPClientWireProfile_TLSCurvesRejectInvalidBeforeNetwork(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		curves []string
		want   string
	}{
		{name: "unknown", curves: []string{"P-224"}, want: `unsupported TLS curve "P-224"`},
		{name: "case-sensitive", curves: []string{"x25519"}, want: `unsupported TLS curve "x25519"`},
		{name: "duplicate", curves: []string{"X25519", "X25519"}, want: `duplicate TLS curve "X25519"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var dialed atomic.Bool
			transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				dialed.Store(true)
				return nil, nil
			}}
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			client := New().newHTTPClient(nil)
			_, errDo := client.Do(ctx, pluginapi.HTTPRequest{
				URL:         "https://127.0.0.1:1",
				WireProfile: &pluginapi.HTTPWireProfile{TLSCurves: test.curves},
			})
			if errDo == nil || !strings.Contains(errDo.Error(), test.want) {
				t.Fatalf("error = %v, want %q", errDo, test.want)
			}
			if dialed.Load() {
				t.Fatal("network dialed before TLS curve validation")
			}
		})
	}
}

func TestHostHTTPClientWireProfile_TLSCurvesRejectCustomTLSDialers(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"DialTLSContext", "DialTLS"} {
		t.Run(name, func(t *testing.T) {
			var dialed atomic.Bool
			transport := &http.Transport{}
			if name == "DialTLSContext" {
				transport.DialTLSContext = func(context.Context, string, string) (net.Conn, error) {
					dialed.Store(true)
					return nil, nil
				}
			} else {
				transport.DialTLS = func(string, string) (net.Conn, error) {
					dialed.Store(true)
					return nil, nil
				}
			}
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
			client := New().newHTTPClient(nil)
			_, errDo := client.Do(ctx, pluginapi.HTTPRequest{
				URL:         "https://127.0.0.1:1",
				WireProfile: &pluginapi.HTTPWireProfile{TLSCurves: []string{"X25519"}},
			})
			if errDo == nil || !strings.Contains(errDo.Error(), "TLS curve profile is not supported with a custom TLS dialer") {
				t.Fatalf("error = %v", errDo)
			}
			if dialed.Load() {
				t.Fatal("custom TLS dialer was called")
			}
		})
	}
}

func TestHostHTTPClientWireProfile_TLSCurvesRejectHTTPSProxyDialer(t *testing.T) {
	t.Parallel()

	client := New().newHTTPClient(&coreauth.Auth{ProxyURL: "https://127.0.0.1:1"})
	_, errDo := client.Do(context.Background(), pluginapi.HTTPRequest{
		URL:         "https://example.com",
		WireProfile: &pluginapi.HTTPWireProfile{TLSCurves: []string{"X25519"}},
	})
	if errDo == nil || !strings.Contains(errDo.Error(), "TLS curve profile is not supported with the configured HTTPS proxy TLS dialer") {
		t.Fatalf("error = %v", errDo)
	}
}

func TestHostHTTPClientWireProfile_TLSCurvesThroughHTTPProxy(t *testing.T) {
	t.Parallel()

	clientHellos := make(chan []tls.CurveID, 1)
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "proxied")
	}))
	backend.TLS = &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			clientHellos <- append([]tls.CurveID(nil), hello.SupportedCurves...)
			return nil, nil
		},
	}
	backend.StartTLS()
	t.Cleanup(backend.Close)

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		upstream, errDial := net.Dial("tcp", request.Host)
		if errDial != nil {
			http.Error(w, errDial.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = upstream.Close()
			http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
			return
		}
		downstream, _, errHijack := hijacker.Hijack()
		if errHijack != nil {
			_ = upstream.Close()
			return
		}
		_, _ = downstream.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() {
			_, _ = io.Copy(upstream, downstream)
			_ = upstream.Close()
		}()
		_, _ = io.Copy(downstream, upstream)
		_ = downstream.Close()
	}))
	t.Cleanup(proxy.Close)

	baseTransport := backend.Client().Transport.(*http.Transport).Clone()
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", baseTransport)
	client := New().newHTTPClient(&coreauth.Auth{ProxyURL: proxy.URL})
	response, errDo := client.Do(ctx, pluginapi.HTTPRequest{
		URL:         backend.URL,
		WireProfile: &pluginapi.HTTPWireProfile{TLSCurves: []string{"P-256", "P-384"}},
	})
	if errDo != nil {
		t.Fatalf("Do through HTTP proxy: %v", errDo)
	}
	if string(response.Body) != "proxied" {
		t.Fatalf("body = %q, want proxied", response.Body)
	}
	select {
	case got := <-clientHellos:
		want := []tls.CurveID{tls.CurveP256, tls.CurveP384}
		if !slices.Equal(got, want) {
			t.Fatalf("proxied ClientHello curves = %v, want %v", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for proxied ClientHello")
	}
}
