# Native loader backends

The `plugin_purego` backend is an **opt-in migration candidate**, not a claim of
production parity. Keep the default backend for production until platform,
compatibility, lifecycle and full-proxy performance acceptance checks pass.

| Build | Backend |
| --- | --- |
| Windows, either CGO setting | Existing Windows DLL loader |
| Linux/macOS amd64/arm64, `-tags plugin_purego` | purego v0.11.0, either CGO setting |
| Linux/macOS/FreeBSD, CGO enabled, otherwise | Existing CGO loader |
| Other builds | No native loader |

Android/iOS are excluded from the purego backend. FreeBSD and other Unix
architectures retain their existing CGO path; no purego parity is claimed.
The support header describes compiled loader capability, not the CGO flag or
whether a particular library and its dependencies can load.

```sh
CGO_ENABLED=0 go build -tags plugin_purego -o cli-proxy-api ./cmd/server
CGO_ENABLED=1 go build -o cli-proxy-api ./cmd/server # default/control
```

Use the namespaced tag, not `purego`: dependencies use that generic tag to
disable unrelated assembly implementations, which would confound A/B tests.
No release or Docker defaults are switched by this change. Ordinary no-CGO Unix
archives remain without plugins; Windows already supports DLLs without CGO.
Linux purego executables still require compatible dynamic libc/libdl/pthread
libraries; no static, musl, Alpine, or OpenWrt portability is implied. Go
`c-shared` plugins still require CGO to build. Existing ABI v1 plugin binaries and
RPC schemas are unchanged; plugins are not recompiled for the host backend.

## Ownership and retirement

Calls use integer/pointer ABI wrappers, `RTLD_NOW|RTLD_LOCAL`, and native-owned
storage. Incoming bytes are copied before use. Plugin outputs are freed through
the plugin's allocator; host outputs through the host's allocator. Two permanent
callback trampolines dispatch by non-reused opaque IDs. Callback panics return
ABI status 1; invalid native memory and native crashes remain process-fatal.

**purego never calls `dlclose`, even after failed initialization or validation.**
Constructors have already executed when `dlopen` returns. The host retires
callback entries immediately when its client guard detaches; already-admitted
callbacks may finish. The existing guard retains native call buffers until
delayed calls actually return. Native shutdown runs once after calls drain.
Host-owned output buffers remain freeable after retirement. ABI records and
library mappings remain valid for native producers until process exit.

An image or initializer can only be admitted once per process, including failed
initialization. Reopening a mapped plugin could replace its module-global host
API and let old producer threads borrow a new identity. Duplicate handles and
initializers are rejected: **restart to retry, re-enable, or reload that image**.
The loader permits at most 1,024 lifetime open attempts, including failures;
afterward restart is required. Immutable registration storage is a fixed 64 KiB
native arena; image/initializer claims are also bounded by this limit. Do not
truncate or overwrite mapped library inodes in place.

These bounds cover loader bookkeeping, not arbitrary plugin threads, runtimes,
or allocations. Distinct plugins must not share mutable host-binding globals in
dependencies. Native plugins are trusted process code, not sandboxed extensions.
Use a process restart for reclamation; reliable hot replacement or fault
isolation requires a separate process boundary, outside this loader migration.

## Validation and A/B measurements

`loader_native_test.go` builds a reviewed C fixture using `cc`; it is shared by
both backends. Tests cover buffers, allocator pairing, JSON envelopes, ABI
validation, native-thread HTTP callbacks, reentrant model calls and streaming.
`loader_purego_test.go` covers layouts, retirement, delayed return, duplicate
images/initializers, failed init and the bounded load budget.

```sh
go test ./...
CGO_ENABLED=0 go test -tags plugin_purego ./...
go test -race -tags plugin_purego ./internal/pluginhost
CGO_ENABLED=0 go vet -tags plugin_purego ./...
```

For controlled comparisons, compile the same fixture once and pass its absolute
path in `CLIPROXY_TEST_NATIVE_PLUGIN` and its reviewed SHA-256 in
`CLIPROXY_TEST_NATIVE_SHA256`. Build separate test binaries from the same source
with the same compiler, `-trimpath`, and CGO setting, changing only
`-tags plugin_purego`; additionally test purego with CGO disabled. Run one
benchmark per fresh process (no per-process reinitialization):

```sh
go test -c -trimpath -o /tmp/cgo.test ./internal/pluginhost
go test -c -trimpath -tags plugin_purego -o /tmp/purego.test ./internal/pluginhost
/tmp/cgo.test -test.run='^$' -test.bench='^BenchmarkNativeCall$/^echo$' -test.benchmem
/tmp/purego.test -test.run='^$' -test.bench='^BenchmarkNativeCall$/^echo$' -test.benchmem
```

Also run `noop`, `host.missing`, and `BenchmarkNativeStream`. Opt-in tests
`CLIPROXY_TEST_STREAM_LOAD=1` and `CLIPROXY_TEST_LOAD_TIMING=1` report paced
32-stream CPU/interchunk latency/RSS/PSS and process-cold load/warm-reopen
behavior respectively. Warm rejection is not a successful load benchmark.
These measurements exercise native/JSON/guard/stream-bridge overhead, not a full
proxy deployment; they cannot alone certify the 5% proxy CPU or 1 ms added p99
interchunk budgets. Do not execute arbitrary downloaded plugins: review and pin
source/artifacts first. `TestNativePinnedExample` runs a checksummed unchanged
model example in a fresh process without attempting Go-library unloading.

`CLIPROXY_TEST_JSHANDLER=1` enables `TestNativePinnedJSHandler` and
`BenchmarkNativeJSHandler` for reviewed, source-built JS Handler v1.0.1 (the
source commit is pinned in the test). It checks no-script request/stream
passthrough and a local script invoking the host log callback. Supply the same
library path/checksum variables; run each case in a fresh process.
