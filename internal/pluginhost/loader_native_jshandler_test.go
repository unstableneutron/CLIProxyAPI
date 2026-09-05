//go:build (cgo || plugin_purego) && (linux || darwin) && !android && !ios && (amd64 || arm64)

package pluginhost

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

// Compatibility target: router-for-me/cpa-plugin-jshandler v1.0.1,
// commit 81a6fcce4c0cc2b62842406a2d21763982b9558f. Build reviewed source
// unchanged, then supply its library path and checksum through the pinned test
// environment. Each test/benchmark must run in a fresh process.
func openNativeJSHandler(t testing.TB) (*guardedPluginClient, string) {
	t.Helper()
	if os.Getenv("CLIPROXY_TEST_JSHANDLER") != "1" {
		t.Skip("set CLIPROXY_TEST_JSHANDLER=1 and the pinned library path/checksum")
	}
	path := pinnedNativePlugin(t)
	client, errOpen := defaultPluginLoader().Open(pluginFile{ID: "jshandler", Path: path}, New())
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	guard := newGuardedPluginClient(client)
	// Deliberately no dlclose of a Go c-shared runtime in the CGO control.
	dir := t.TempDir()
	req, errMarshal := json.Marshal(map[string]any{"plugin_dir": dir, "config_yaml": []byte("enabled: true\nscript_paths: []\n")})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	out, errCall := guard.Call(context.Background(), "plugin.register", req)
	if errCall != nil || !bytes.Contains(out, []byte(`"ok":true`)) {
		t.Fatalf("jshandler registration: %s %v", out, errCall)
	}
	return guard, dir
}

func TestNativePinnedJSHandler(t *testing.T) {
	client, dir := openNativeJSHandler(t)
	request := []byte(`{"RequestID":"fixture","SourceFormat":"openai","Model":"fixture","Body":"eyJtZXNzYWdlcyI6W119"}`)
	stream := []byte(`{"RequestID":"fixture","SourceFormat":"openai","Model":"fixture","ChunkIndex":0,"Body":"ZGF0YTogW0RPTkVdCgo="}`)
	for method, req := range map[string][]byte{"request.intercept_before": request, "response.intercept_stream_chunk": stream} {
		out, errCall := client.Call(context.Background(), method, req)
		if errCall != nil || !bytes.Contains(out, []byte(`"Body":null`)) || !bytes.Contains(out, []byte(`"ok":true`)) {
			t.Fatalf("no-script %s: %s %v", method, out, errCall)
		}
		t.Logf("%s: %s", method, out)
	}
	if errDir := os.Mkdir(filepath.Join(dir, "scripts"), 0o700); errDir != nil {
		t.Fatal(errDir)
	}
	script := `function on_before_request(ctx) { console.log("native-abi-callback"); ctx.headers["X-Native-Test"] = "yes"; return ctx; }`
	if errWrite := os.WriteFile(filepath.Join(dir, "scripts", "fixture.js"), []byte(script), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	var logs bytes.Buffer
	oldOutput, oldLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&logs)
	log.SetLevel(log.InfoLevel)
	defer func() { log.SetOutput(oldOutput); log.SetLevel(oldLevel) }()
	out, errCall := client.Call(context.Background(), "request.intercept_before", request)
	if errCall != nil {
		t.Fatal(errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.RequestInterceptResponse](out)
	if errDecode != nil || resp.Headers.Get("X-Native-Test") != "yes" || !strings.Contains(logs.String(), "native-abi-callback") {
		t.Fatalf("Go plugin callback/script: %s %v; host callback observed=%v", out, errDecode, strings.Contains(logs.String(), "native-abi-callback"))
	}
	t.Logf("script request: %s", out)
}

func BenchmarkNativeJSHandler(b *testing.B) {
	for _, method := range []string{"request.intercept_before", "response.intercept_stream_chunk"} {
		b.Run(method, func(b *testing.B) {
			client, _ := openNativeJSHandler(b)
			req := []byte(`{"RequestID":"fixture","SourceFormat":"openai","Model":"fixture","ChunkIndex":0,"Body":"ZGF0YTogW0RPTkVdCgo="}`)
			b.ReportAllocs()
			b.SetBytes(int64(len(req)))
			b.ResetTimer()
			for b.Loop() {
				if out, errCall := client.Call(context.Background(), method, req); errCall != nil || !bytes.Contains(out, []byte(`"ok":true`)) {
					b.Fatalf("jshandler: %s %v", out, errCall)
				}
			}
		})
	}
}
