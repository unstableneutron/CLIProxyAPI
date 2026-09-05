//go:build plugin_purego && (linux || darwin) && !android && !ios && (amd64 || arm64)

package pluginhost

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func TestPuregoLayout(t *testing.T) {
	if unsafe.Sizeof(nativeBuffer{}) != 16 || unsafe.Offsetof(nativeBuffer{}.len) != 8 ||
		unsafe.Sizeof(nativeHostAPI{}) != 32 || unsafe.Offsetof(nativeHostAPI{}.hostCtx) != 8 ||
		unsafe.Offsetof(nativeHostAPI{}.call) != 16 || unsafe.Offsetof(nativeHostAPI{}.freeBuffer) != 24 ||
		unsafe.Sizeof(nativePluginAPI{}) != 32 || unsafe.Offsetof(nativePluginAPI{}.call) != 8 ||
		unsafe.Offsetof(nativePluginAPI{}.freeBuffer) != 16 || unsafe.Offsetof(nativePluginAPI{}.shutdown) != 24 {
		t.Fatal("ABI v1 native layouts changed")
	}
	if SupportPluginHeaderValue() != "1" {
		t.Fatal("purego loader capability not advertised")
	}
}

func TestPuregoRetirement(t *testing.T) {
	path := buildNativeFixture(t)
	inner, errOpen := defaultPluginLoader().Open(pluginFile{ID: "old", Path: path}, New())
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	client := inner.(*dynamicLibraryClient)
	guard := newGuardedPluginClient(client)
	t.Cleanup(guard.Shutdown)
	// A native producer can still execute mapped code after logical retirement.
	// This probe invokes the retained ABI directly, not the public client guard.
	probe := &dynamicLibraryClient{runtime: client.runtime, api: client.api}
	if _, errCall := guard.Call(context.Background(), "hold", nil); errCall != nil {
		t.Fatal(errCall)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, errCall := guard.Call(ctx, "delay", []byte("delayed"))
		returned <- errCall
	}()
	deadline := time.Now().Add(time.Second)
	for {
		out, errCall := probe.Call(context.Background(), "stats", nil)
		if errCall != nil {
			t.Fatal(errCall)
		}
		if strings.HasSuffix(string(out), ",1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native call did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if errCall := <-returned; errCall != context.Canceled {
		t.Fatalf("cancellation: %v", errCall)
	}
	guard.ShutdownContext(ctx)
	if _, exists := client.runtime.entries.Load(client.id); exists {
		t.Fatal("retired callback still registered")
	}
	// A rejected reload must not replace the module-global host API.
	if _, errReload := defaultPluginLoader().Open(pluginFile{ID: "new", Path: path}, New()); errReload == nil {
		t.Fatal("reinitialized a still-mapped image")
	}
	for range 10 {
		out, errCall := probe.Call(context.Background(), "late", nil)
		if errCall != nil || string(out) != "1" {
			t.Fatalf("late callback accepted: %s %v", out, errCall)
		}
	}
	if _, errCall := probe.Call(context.Background(), "free-held", nil); errCall != nil {
		t.Fatal(errCall)
	}
	select {
	case <-guard.shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("native delayed return did not drain within 1s")
	}
	out, errStats := probe.Call(context.Background(), "stats", nil)
	if errStats != nil || string(out) != "0,1,1,1" {
		t.Fatalf("native lifetime/allocator counters: %s %v", out, errStats)
	}
}

func TestPuregoConcurrentReopen(t *testing.T) {
	path := buildNativeFixture(t)
	alias := filepath.Join(filepath.Dir(path), "alias.so")
	if errLink := os.Symlink(path, alias); errLink != nil {
		t.Fatal(errLink)
	}
	var wg sync.WaitGroup
	winners := make(chan pluginClient, 16)
	for i := range 16 {
		wg.Go(func() {
			name := path
			if i%2 == 0 {
				name = alias
			}
			client, errOpen := defaultPluginLoader().Open(pluginFile{ID: "fixture", Path: name}, New())
			if errOpen == nil {
				winners <- client
			}
		})
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("initializer winners = %d, want 1", len(winners))
	}
	for client := range winners {
		client.Shutdown()
	}
}

func TestPuregoFailedInitRetainsIdentity(t *testing.T) {
	path := buildNativeFixture(t, "FAIL_INIT")
	r, errRuntime := nativeRuntime()
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	before := r.loads.Load()
	if _, errOpen := defaultPluginLoader().Open(pluginFile{ID: "bad", Path: path}, New()); errOpen == nil || !strings.Contains(errOpen.Error(), "returned -3") {
		t.Fatalf("failed init: %v", errOpen)
	}
	if _, exists := r.entries.Load(uintptr(before) + 1); exists {
		t.Fatal("failed init callback entry leaked")
	}
	if _, errOpen := defaultPluginLoader().Open(pluginFile{ID: "retry", Path: path}, New()); errOpen == nil || !strings.Contains(errOpen.Error(), "already opened") {
		t.Fatalf("failed initializer retried: %v", errOpen)
	}
}

func TestPuregoSharedInitializer(t *testing.T) {
	dependency := buildNativeFixture(t)
	var first pluginClient
	for i := range 2 {
		path := filepath.Join(filepath.Dir(dependency), "wrapper"+string(rune('a'+i))+".so")
		cmd := exec.Command("cc", "-shared", "-fPIC", "-o", path, "testdata/native_wrapper.c", dependency)
		if out, errBuild := cmd.CombinedOutput(); errBuild != nil {
			t.Fatalf("build wrapper: %v\n%s", errBuild, out)
		}
		client, errOpen := defaultPluginLoader().Open(pluginFile{ID: "wrapper", Path: path}, New())
		if i == 0 {
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			first = client
			t.Cleanup(first.Shutdown)
		} else if errOpen == nil || !strings.Contains(errOpen.Error(), "initializer already used") {
			t.Fatalf("shared initializer was not rejected: %v", errOpen)
		}
	}
}

func TestPuregoLoadBudget(t *testing.T) {
	// Exhaustion is process-wide and irreversible. Isolate it from other tests.
	if os.Getenv("CLIPROXY_TEST_LOAD_BUDGET") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPuregoLoadBudget$")
		cmd.Env = append(os.Environ(), "CLIPROXY_TEST_LOAD_BUDGET=1")
		if out, errRun := cmd.CombinedOutput(); errRun != nil {
			t.Fatalf("load budget subprocess: %v\n%s", errRun, out)
		}
		return
	}
	path := buildNativeFixture(t)
	client, errOpen := defaultPluginLoader().Open(pluginFile{ID: "fixture", Path: path}, New())
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	r, _ := nativeRuntime()
	call, free, records := r.callCallback, r.freeCallback, r.records
	client.Shutdown()
	for range nativeLoadLimit + 10 {
		if _, errReload := defaultPluginLoader().Open(pluginFile{ID: "reload", Path: path}, New()); errReload == nil {
			t.Fatal("unexpected reload success")
		}
	}
	if r.loads.Load() != nativeLoadLimit || r.callCallback != call || r.freeCallback != free || r.records != records {
		t.Fatal("unbounded load count or per-load callback/storage allocation")
	}
	r.entries.Range(func(_, _ any) bool {
		t.Error("retired host entry leaked")
		return true
	})
}

func TestPuregoInvalidBufferAndPanic(t *testing.T) {
	if _, errCopy := copyNativeBuffer(nil, 1); errCopy == nil {
		t.Fatal("accepted NULL nonempty buffer")
	}
	if _, errCopy := copyNativeBuffer(nil, ^uintptr(0)); errCopy == nil {
		t.Fatal("accepted overflowing buffer")
	}
	r, errRuntime := nativeRuntime()
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	// A corrupted Go dispatch entry provokes a recoverable type assertion panic.
	// Native pointer faults are deliberately not tested in this process.
	id := ^uintptr(0)
	r.entries.Store(id, "invalid entry")
	defer r.entries.Delete(id)
	method := []byte("missing\x00")
	var response nativeBuffer
	if nativeHostCall(id, unsafe.Pointer(&method[0]), nil, 0, &response) != 1 {
		t.Fatal("callback panic crossed the ABI error boundary")
	}
}
