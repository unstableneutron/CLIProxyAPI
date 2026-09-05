//go:build plugin_purego && (linux || darwin) && !android && !ios && (amd64 || arm64)

package pluginhost

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

// These layouts are ABI v1, not Go objects shared with a plugin runtime.
type nativeBuffer struct {
	ptr unsafe.Pointer
	len uintptr
}

type nativeHostAPI struct {
	abiVersion uint32
	hostCtx    uintptr
	call       uintptr
	freeBuffer uintptr
}

type nativePluginAPI struct {
	abiVersion uint32
	call       uintptr
	freeBuffer uintptr
	shutdown   uintptr
}

type nativeRegistration struct {
	host   nativeHostAPI
	plugin nativePluginAPI
}

// Go c-shared libraries cannot safely be dlclosed, including after failed init.
// Bound lifetime load attempts and retained ABI storage rather than silently
// growing tombstones forever. Exhaustion requires a process restart.
const nativeLoadLimit = 1024

var (
	nativeRuntimeOnce  sync.Once
	nativeRuntimeState *nativeLoaderRuntime
	nativeRuntimeError error
)

func nativeRuntime() (*nativeLoaderRuntime, error) {
	nativeRuntimeOnce.Do(func() {
		nativeRuntimeState, nativeRuntimeError = newNativeRuntime()
	})
	return nativeRuntimeState, nativeRuntimeError
}

func newNativeRuntime() (*nativeLoaderRuntime, error) {
	calloc, errCalloc := purego.Dlsym(purego.RTLD_DEFAULT, "calloc")
	if errCalloc != nil {
		return nil, fmt.Errorf("resolve calloc: %w", errCalloc)
	}
	free, errFree := purego.Dlsym(purego.RTLD_DEFAULT, "free")
	if errFree != nil {
		return nil, fmt.Errorf("resolve free: %w", errFree)
	}
	r := &nativeLoaderRuntime{calloc: calloc, free: free}
	r.records = r.allocate(nativeLoadLimit * unsafe.Sizeof(nativeRegistration{}))
	if r.records == nil {
		return nil, fmt.Errorf("allocate native plugin registrations")
	}
	// Only two permanent purego trampolines, independent of reload/request count.
	r.callCallback = purego.NewCallback(nativeHostCall)
	r.freeCallback = purego.NewCallback(nativeHostFree)
	return r, nil
}

type nativeLoaderRuntime struct {
	calloc, free, callCallback, freeCallback uintptr
	records                                  unsafe.Pointer
	loads                                    atomic.Uint32
	entries                                  sync.Map
	handles                                  sync.Map
	initializers                             sync.Map
}

// nativePointer converts an address returned by the native allocator, never a
// stored uintptr derived from a Go pointer. Reinterpretation avoids treating
// foreign addresses as pointers into Go's heap under checkptr.
func nativePointer(address uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&address))
}

func (r *nativeLoaderRuntime) allocate(size uintptr) unsafe.Pointer {
	p, _, _ := purego.SyscallN(r.calloc, 1, size)
	return nativePointer(p)
}

func (r *nativeLoaderRuntime) release(ptr unsafe.Pointer) {
	purego.SyscallN(r.free, uintptr(ptr))
}

type dynamicLibraryLoader struct{}

type dynamicLibraryClient struct {
	runtime *nativeLoaderRuntime
	id      uintptr
	api     nativePluginAPI
	retired atomic.Bool
	once    sync.Once
}

func defaultPluginLoader() pluginLoader { return dynamicLibraryLoader{} }

func (dynamicLibraryLoader) Open(file pluginFile, host *Host) (pluginClient, error) {
	r, errRuntime := nativeRuntime()
	if errRuntime != nil {
		return nil, errRuntime
	}
	var slot uint32
	for {
		slot = r.loads.Load()
		if slot >= nativeLoadLimit {
			return nil, fmt.Errorf("native plugin lifetime load limit (%d) reached; restart the process", nativeLoadLimit)
		}
		if r.loads.CompareAndSwap(slot, slot+1) {
			break
		}
	}
	// Do not hold a Go lock across native code: init/constructors may reenter.
	handle, errOpen := purego.Dlopen(file.Path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if errOpen != nil {
		return nil, fmt.Errorf("dlopen %s: %w", file.Path, errOpen)
	}
	// Deliberately retain the mapping even if symbol/table validation fails:
	// dlopen has already run arbitrary constructors (possibly another Go runtime).
	// Reinitializing a mapped plugin would replace its module-global host API,
	// letting an old producer borrow a new callback identity after retirement.
	if _, loaded := r.handles.LoadOrStore(handle, struct{}{}); loaded {
		return nil, fmt.Errorf("native plugin image already opened; restart the process to reload %s", file.Path)
	}
	init, errSymbol := purego.Dlsym(handle, "cliproxy_plugin_init")
	if errSymbol != nil {
		return nil, fmt.Errorf("missing cliproxy_plugin_init: %w", errSymbol)
	}
	// Dlsym also searches dependencies; distinct handles can share an initializer.
	if _, loaded := r.initializers.LoadOrStore(init, struct{}{}); loaded {
		return nil, fmt.Errorf("native plugin initializer already used; restart the process to reload %s", file.Path)
	}
	id := uintptr(slot) + 1
	record := (*nativeRegistration)(unsafe.Add(r.records, uintptr(slot)*unsafe.Sizeof(nativeRegistration{})))
	record.host = nativeHostAPI{pluginHostABIVersion, id, r.callCallback, r.freeCallback}
	r.entries.Store(id, dynamicHostCallbackEntry{host: host, pluginID: file.ID})
	rc, _, _ := purego.SyscallN(init, uintptr(unsafe.Pointer(&record.host)), uintptr(unsafe.Pointer(&record.plugin)))
	if int32(rc) != 0 {
		r.entries.Delete(id)
		return nil, fmt.Errorf("cliproxy_plugin_init returned %d", int32(rc))
	}
	if record.plugin.abiVersion != pluginHostABIVersion {
		r.entries.Delete(id)
		return nil, fmt.Errorf("plugin ABI version %d is not supported", record.plugin.abiVersion)
	}
	if record.plugin.call == 0 || record.plugin.freeBuffer == 0 {
		r.entries.Delete(id)
		return nil, fmt.Errorf("plugin function table is incomplete")
	}
	return &dynamicLibraryClient{runtime: r, id: id, api: record.plugin}, nil
}

func (c *dynamicLibraryClient) Call(ctx context.Context, method string, request []byte) ([]byte, error) {
	if c == nil || c.retired.Load() {
		return nil, fmt.Errorf("plugin client is closed")
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	// One native allocation contains the response descriptor, NUL-terminated
	// method and copied request. No Go-owned memory crosses the ABI boundary.
	size := unsafe.Sizeof(nativeBuffer{}) + uintptr(len(method)) + 1 + uintptr(len(request))
	mem := c.runtime.allocate(size)
	if mem == nil {
		return nil, fmt.Errorf("allocate plugin call buffer")
	}
	defer c.runtime.release(mem)
	response := (*nativeBuffer)(mem)
	methodPtr := unsafe.Add(mem, unsafe.Sizeof(nativeBuffer{}))
	copy(unsafe.Slice((*byte)(methodPtr), len(method)), method)
	var requestPtr unsafe.Pointer
	if len(request) > 0 {
		requestPtr = unsafe.Add(methodPtr, len(method)+1)
		copy(unsafe.Slice((*byte)(requestPtr), len(request)), request)
	}
	rc, _, _ := purego.SyscallN(c.api.call, uintptr(methodPtr), uintptr(requestPtr), uintptr(len(request)), uintptr(mem))
	if response.ptr != nil {
		defer purego.SyscallN(c.api.freeBuffer, uintptr(response.ptr), response.len)
	}
	out, errCopy := copyNativeBuffer(response.ptr, response.len)
	if errCopy != nil {
		return nil, errCopy
	}
	if int32(rc) != 0 && !isPluginErrorEnvelope(out) {
		return nil, fmt.Errorf("plugin call %s returned %d: %s", method, int32(rc), string(out))
	}
	return out, nil
}

func copyNativeBuffer(ptr unsafe.Pointer, length uintptr) ([]byte, error) {
	if length == 0 {
		return nil, nil
	}
	if ptr == nil || length > uintptr(^uint(0)>>1) {
		return nil, fmt.Errorf("invalid native plugin buffer length %d", length)
	}
	return append([]byte(nil), unsafe.Slice((*byte)(ptr), int(length))...), nil
}

func (c *dynamicLibraryClient) retire() {
	if c != nil {
		c.retired.Store(true)
		c.runtime.entries.Delete(c.id)
	}
}

func (c *dynamicLibraryClient) Shutdown() {
	if c == nil {
		return
	}
	c.retire()
	// The existing client guard drains native calls before invoking Shutdown.
	// Already-admitted callbacks may finish; callbacks arriving now are rejected.
	c.once.Do(func() {
		if c.api.shutdown != 0 {
			purego.SyscallN(c.api.shutdown)
		}
	})
}

func nativeHostCall(id uintptr, method unsafe.Pointer, request unsafe.Pointer, length uintptr, response *nativeBuffer) (status int32) {
	// Go panics must never unwind through a native frame. Invalid native pointers
	// and faults remain process-fatal: this loader is not a sandbox.
	defer func() {
		if recover() != nil {
			status = 1
		}
	}()
	if response != nil {
		*response = nativeBuffer{}
	}
	r, errRuntime := nativeRuntime()
	if errRuntime != nil || id == 0 || method == nil {
		return 1
	}
	raw, ok := r.entries.Load(id)
	if !ok {
		return 1
	}
	entry := raw.(dynamicHostCallbackEntry)
	if entry.host == nil {
		return 1
	}
	req, errCopy := copyNativeBuffer(request, length)
	if errCopy != nil {
		return 1
	}
	var methodLen uintptr
	for *(*byte)(unsafe.Add(method, methodLen)) != 0 {
		methodLen++
	}
	methodName := string(unsafe.Slice((*byte)(method), methodLen))
	ctx := withHostCallbackPluginID(context.Background(), entry.pluginID)
	resp, errCall := entry.host.callFromPlugin(ctx, methodName, req)
	if errCall != nil {
		resp = marshalRPCError("host_call_failed", errCall.Error())
	}
	if response == nil || len(resp) == 0 {
		return 0
	}
	mem := r.allocate(uintptr(len(resp)))
	if mem == nil {
		return 1
	}
	copy(unsafe.Slice((*byte)(mem), len(resp)), resp)
	*response = nativeBuffer{mem, uintptr(len(resp))}
	return 0
}

func nativeHostFree(ptr unsafe.Pointer, _ uintptr) {
	// This callback remains valid after retirement for already-owned buffers.
	if ptr != nil {
		r, _ := nativeRuntime()
		r.release(ptr)
	}
}
