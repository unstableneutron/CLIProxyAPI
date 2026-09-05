//go:build (cgo || plugin_purego) && (linux || darwin) && !android && !ios && (amd64 || arm64)

package pluginhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func buildNativeFixture(t testing.TB, defines ...string) string {
	t.Helper()
	cc, errCC := exec.LookPath("cc")
	if errCC != nil {
		t.Skip("native ABI fixture requires a C compiler")
	}
	path := filepath.Join(t.TempDir(), "fixture.so")
	args := []string{"-shared", "-fPIC", "-O2", "-std=c11", "-D_DEFAULT_SOURCE", "-pthread", "-o", path, "testdata/native_plugin.c"}
	for _, define := range defines {
		args = append(args, "-D"+define)
	}
	if out, errBuild := exec.Command(cc, args...).CombinedOutput(); errBuild != nil {
		t.Fatalf("build native fixture: %v\n%s", errBuild, out)
	}
	return path
}

func openNativeFixture(t testing.TB, host *Host) *guardedPluginClient {
	t.Helper()
	var path string
	if os.Getenv("CLIPROXY_TEST_NATIVE_PLUGIN") != "" {
		path = pinnedNativePlugin(t)
	} else {
		path = buildNativeFixture(t)
	}
	client, errOpen := defaultPluginLoader().Open(pluginFile{ID: "fixture", Path: path}, host)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	guard := newGuardedPluginClient(client)
	t.Cleanup(guard.Shutdown)
	return guard
}

func TestNativeBuffersAndCallbacks(t *testing.T) {
	client := openNativeFixture(t, New())
	for _, size := range []int{0, 1, 256, 1 << 20} {
		input := make([]byte, size)
		for i := range input {
			input[i] = byte(i)
		}
		out, errCall := client.Call(context.Background(), "echo", input)
		if errCall != nil || !bytes.Equal(input, out) {
			t.Fatalf("echo %d: %v", size, errCall)
		}
	}
	if out, errCall := client.Call(context.Background(), "error", nil); errCall != nil || !isPluginErrorEnvelope(out) {
		t.Fatalf("error envelope: %s %v", out, errCall)
	}
	if _, errCall := client.Call(context.Background(), "raw-error", nil); errCall == nil || !strings.Contains(errCall.Error(), "returned -7") {
		t.Fatalf("signed native status: %v", errCall)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Native", "yes")
		_, _ = w.Write([]byte{0, 1, 255})
	}))
	defer server.Close()
	req, errMarshal := json.Marshal(pluginapi.HTTPRequest{Method: http.MethodGet, URL: server.URL})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, method := range []string{"host.http.do", "foreign"} {
		out, errCall := client.Call(context.Background(), method, req)
		if errCall != nil {
			t.Fatal(errCall)
		}
		resp, errDecode := decodeRPCEnvelope[pluginapi.HTTPResponse](out)
		if errDecode != nil || resp.StatusCode != 200 || !bytes.Equal(resp.Body, []byte{0, 1, 255}) || resp.Headers.Get("X-Native") != "yes" {
			t.Fatalf("%s: %s %v", method, out, errDecode)
		}
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				out, errCall := client.Call(context.Background(), "echo", []byte(`{"request":"concurrent"}`))
				if errCall != nil || string(out) != `{"request":"concurrent"}` {
					t.Errorf("concurrent echo: %s %v", out, errCall)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for range 10 {
			runtime.GC()
		}
	})
	wg.Wait()
	out, errStats := client.Call(context.Background(), "stats", nil)
	if errStats != nil || string(out) != "0,1,0,0" {
		t.Fatalf("allocator pairing/init count: %s %v", out, errStats)
	}
}

func TestNativeValidation(t *testing.T) {
	for define, want := range map[string]string{"BAD_ABI": "ABI version 99", "BAD_TABLE": "table is incomplete", "FAIL_INIT": "returned -3", "NO_INIT": "missing cliproxy_plugin_init"} {
		t.Run(define, func(t *testing.T) {
			path := buildNativeFixture(t, define)
			_, errOpen := defaultPluginLoader().Open(pluginFile{ID: "bad", Path: path}, New())
			if errOpen == nil || !strings.Contains(errOpen.Error(), want) {
				t.Fatalf("want %q, got %v", want, errOpen)
			}
		})
	}
}

func TestNativeMalformedRegistration(t *testing.T) {
	path := buildNativeFixture(t, "BAD_REGISTRATION")
	host := New()
	client, errOpen := defaultPluginLoader().Open(pluginFile{ID: "bad", Path: path}, host)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	guard := newGuardedPluginClient(client)
	defer guard.Shutdown()
	if _, errRegister := registerRPCPlugin(context.Background(), host, "bad", guard, pluginabi.MethodPluginRegister, nil); errRegister == nil {
		t.Fatal("accepted malformed registration JSON")
	}
}

func TestNativeModelReentrancyAndStream(t *testing.T) {
	host := New()
	client := openNativeFixture(t, host)
	host.SetModelExecutor(&fakeHostModelExecutor{
		executeModel: func(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *interfaces.ErrorMessage) {
			// Reenter the same native plugin while its host callback is active.
			out, errCall := client.Call(ctx, "echo", req.Body)
			if errCall != nil {
				t.Error(errCall)
			}
			return handlers.ModelExecutionResponse{StatusCode: 200, Body: out}, nil
		},
		executeModelStream: func(ctx context.Context, req handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *interfaces.ErrorMessage) {
			chunks := make(chan handlers.ModelExecutionChunk, 2)
			chunks <- handlers.ModelExecutionChunk{Payload: []byte("data: first\n\n")}
			chunks <- handlers.ModelExecutionChunk{Payload: []byte("data: second\n\n")}
			close(chunks)
			return handlers.ModelExecutionStream{StatusCode: 200, Chunks: chunks}, nil
		},
	})
	req, errMarshal := json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai", ExitProtocol: "openai", Model: "fixture", Body: []byte(`{"request":true}`),
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	out, errCall := client.Call(context.Background(), pluginabi.MethodHostModelExecute, req)
	if errCall != nil {
		t.Fatal(errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelExecutionResponse](out)
	if errDecode != nil || resp.StatusCode != 200 || string(resp.Body) != `{"request":true}` {
		t.Fatalf("reentrant model response: %s %v", out, errDecode)
	}
	req, errMarshal = json.Marshal(pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai", ExitProtocol: "openai", Model: "fixture", Stream: true, Body: []byte(`{"stream":true}`),
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	out, errCall = client.Call(context.Background(), pluginabi.MethodHostModelExecuteStream, req)
	if errCall != nil {
		t.Fatal(errCall)
	}
	stream, errStream := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](out)
	if errStream != nil || stream.StreamID == "" {
		t.Fatalf("model stream: %s %v", out, errStream)
	}
	readReq := []byte(fmt.Sprintf(`{"stream_id":%q}`, stream.StreamID))
	var combined []byte
	for range 3 {
		out, errCall = client.Call(context.Background(), pluginabi.MethodHostModelStreamRead, readReq)
		if errCall != nil {
			t.Fatal(errCall)
		}
		chunk, errChunk := decodeRPCEnvelope[struct {
			Payload []byte `json:"payload"`
			Done    bool   `json:"done"`
		}](out)
		if errChunk != nil {
			t.Fatal(errChunk)
		}
		combined = append(combined, chunk.Payload...)
		if chunk.Done {
			break
		}
	}
	if string(combined) != "data: first\n\ndata: second\n\n" {
		t.Fatalf("stream payload: %q", combined)
	}
}

// Run one unchanged source-built example per process. Do not dlclose Go
// c-shared libraries in the CGO control: Go does not support safe unloading.
func TestNativePinnedExample(t *testing.T) {
	path := pinnedNativePlugin(t)
	client, errOpen := defaultPluginLoader().Open(pluginFile{ID: "example", Path: path}, New())
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	guard := newGuardedPluginClient(client)
	for _, method := range []string{"plugin.register", "model.static", "model.for_auth"} {
		out, errCall := guard.Call(context.Background(), method, []byte(`{}`))
		if errCall != nil || !json.Valid(out) || !bytes.Contains(out, []byte(`"ok":true`)) {
			t.Fatalf("%s: %s %v", method, out, errCall)
		}
		t.Logf("%s: %s", method, out)
	}
}

func pinnedNativePlugin(t testing.TB) string {
	t.Helper()
	path := os.Getenv("CLIPROXY_TEST_NATIVE_PLUGIN")
	if path == "" {
		t.Skip("set CLIPROXY_TEST_NATIVE_PLUGIN to a reviewed source-built model example")
	}
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if expected := os.Getenv("CLIPROXY_TEST_NATIVE_SHA256"); expected == "" || digest != expected {
		t.Fatal("a matching CLIPROXY_TEST_NATIVE_SHA256 is required before executing this library")
	}
	t.Logf("plugin sha256=%s", digest)
	return path
}

func BenchmarkNativeCall(b *testing.B) {
	for _, method := range []string{"noop", "echo", "host.missing"} {
		b.Run(method, func(b *testing.B) {
			client := openNativeFixture(b, New())
			req := []byte(`{"id":"request-1","chunk_index":42,"payload":"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"}`)
			b.SetBytes(int64(len(req)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, errCall := client.Call(context.Background(), method, req); errCall != nil {
					b.Fatal(errCall)
				}
			}
		})
	}
}
