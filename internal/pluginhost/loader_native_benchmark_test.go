//go:build (cgo || plugin_purego) && (linux || darwin) && !android && !ios && (amd64 || arm64)

package pluginhost

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"golang.org/x/sys/unix"
)

func BenchmarkNativeStream(b *testing.B) {
	host := New()
	client := openNativeFixture(b, host)
	payload := bytes.Repeat([]byte("data: chunk\n\n"), 8)
	chunks := make(chan pluginapi.HTTPStreamChunk, 1)
	id := host.httpStreams.open(chunks, nil)
	defer host.httpStreams.close(id)
	req := []byte(fmt.Sprintf(`{"stream_id":%q}`, id))
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		chunks <- pluginapi.HTTPStreamChunk{Payload: payload}
		out, errCall := client.Call(context.Background(), "host.http.stream_read", req)
		if errCall != nil {
			b.Fatal(errCall)
		}
		response, errDecode := decodeRPCEnvelope[rpcHostHTTPStreamReadResponse](out)
		if errDecode != nil || !bytes.Equal(response.Payload, payload) {
			b.Fatalf("stream payload mismatch: %v", errDecode)
		}
	}
}

// This measures the native/JSON/client-guard/stream-bridge path, NOT a complete
// proxy deployment. Run each backend in a separate process on an idle machine.
func TestNativeStreamLoad(t *testing.T) {
	if os.Getenv("CLIPROXY_TEST_STREAM_LOAD") != "1" {
		t.Skip("set CLIPROXY_TEST_STREAM_LOAD=1 for the paced stream workload")
	}
	host := New()
	client := openNativeFixture(t, host)
	const streams, chunksPerStream = 32, 256
	payload := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
	var before, after unix.Rusage
	if errUsage := unix.Getrusage(unix.RUSAGE_SELF, &before); errUsage != nil {
		t.Fatal(errUsage)
	}
	start := time.Now()
	intervals := make(chan time.Duration, streams*(chunksPerStream-1))
	var wg sync.WaitGroup
	for range streams {
		wg.Go(func() {
			chunks := make(chan pluginapi.HTTPStreamChunk, 1)
			id := host.httpStreams.open(chunks, nil)
			defer host.httpStreams.close(id)
			req := []byte(fmt.Sprintf(`{"stream_id":%q}`, id))
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			var previous time.Time
			for range chunksPerStream {
				<-ticker.C
				chunks <- pluginapi.HTTPStreamChunk{Payload: payload}
				out, errCall := client.Call(context.Background(), "host.http.stream_read", req)
				if errCall != nil {
					t.Error(errCall)
					return
				}
				resp, errDecode := decodeRPCEnvelope[rpcHostHTTPStreamReadResponse](out)
				if errDecode != nil || !bytes.Equal(resp.Payload, payload) {
					t.Errorf("stream corruption: %v", errDecode)
					return
				}
				now := time.Now()
				if !previous.IsZero() {
					intervals <- now.Sub(previous)
				}
				previous = now
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	if errUsage := unix.Getrusage(unix.RUSAGE_SELF, &after); errUsage != nil {
		t.Fatal(errUsage)
	}
	close(intervals)
	var samples []time.Duration
	for sample := range intervals {
		samples = append(samples, sample)
	}
	if len(samples) != streams*(chunksPerStream-1) {
		t.Fatalf("missing stream samples: %d", len(samples))
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	cpu := after.Utime.Nano() + after.Stime.Nano() - before.Utime.Nano() - before.Stime.Nano()
	t.Logf("streams=%d chunks=%d elapsed=%s cpu=%s throughput=%.1f chunks/s interchunk p50=%s p95=%s p99=%s",
		streams, streams*chunksPerStream, elapsed, time.Duration(cpu), float64(streams*chunksPerStream)/elapsed.Seconds(),
		samples[len(samples)*50/100], samples[len(samples)*95/100], samples[len(samples)*99/100])
	if memory, errRead := os.ReadFile("/proc/self/smaps_rollup"); errRead == nil {
		for _, line := range strings.Split(string(memory), "\n") {
			if strings.HasPrefix(line, "Rss:") || strings.HasPrefix(line, "Pss:") {
				t.Log(line)
			}
		}
	}
}

func TestNativeLoadTiming(t *testing.T) {
	if os.Getenv("CLIPROXY_TEST_LOAD_TIMING") != "1" {
		t.Skip("set CLIPROXY_TEST_LOAD_TIMING=1 for process-cold and warm loader measurements")
	}
	path := pinnedNativePlugin(t)
	host := New()
	start := time.Now()
	client, errOpen := defaultPluginLoader().Open(pluginFile{ID: "cold", Path: path}, host)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Logf("process-cold open+init=%s", time.Since(start))
	client.Shutdown()
	start = time.Now()
	client, errOpen = defaultPluginLoader().Open(pluginFile{ID: "warm", Path: path}, host)
	// Purego intentionally rejects reinitializing a mapped image; do not label
	// that rejection as a successful warm load or compare the timings as parity.
	t.Logf("warm reopen=%s error=%v", time.Since(start), errOpen)
	if errOpen == nil {
		client.Shutdown()
	}
}
