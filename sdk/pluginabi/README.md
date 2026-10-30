# Native plugin contract extensions

Upstream CLIProxyAPI assigns its own features to schemas 7-9: request-body
omission (7), request-body patching (8) and multi-provider model registrations
(9). This host supports both upstream's and the fork's features at the same
numbers. Every feature is opt-in through its own capability or field, so a
plugin declaring schema 9 gets only what it declares.

Schema 7 adds typed failure details without changing native ABI v1. A plugin
that depends on these semantics must return `schema_version: 7` at registration
so older hosts reject it rather than silently losing credential-health evidence.
Older plugin schemas and plain string stream errors remain supported.

RPC error envelopes use the `Error` fields defined in `types.go`. Streaming
`host.stream.emit` and `host.stream.close` callbacks may send the same object
under `failure`, alongside `stream_id`. When both `failure` and the legacy
`error` string are present, `failure` takes precedence.

```json
{
  "stream_id": "1",
  "failure": {
    "code": "provider_aborted",
    "message": "Generation was aborted",
    "http_status": 400,
    "scope": "request",
    "retryable": false
  }
}
```

`request` scope stops request-local failure from cooling credentials;
`credential` scope supplies credential-wide classification to the host.
`model`, empty and unknown scopes retain normal host status-based decisions.
`retry_after_ms` is an optional nonnegative relative delay; zero is distinct
from omission. Negative or overflowing durations are ignored. `retryable` is
preserved as evidence, not authority to override host retry/commitment policy.
The host continues to own cooldown, eligibility and semantic-commit decisions.

For compatibility with upstream plugins, the host also accepts upstream's
`retry_after_seconds` field and the `error_detail` stream-callback key. A valid
`retry_after_ms` (including zero) wins over `retry_after_seconds`, and `failure`
wins over `error_detail`; positive hints also yield a whole-second
`Retry-After` response header, rounded up.

## Authenticated HTTP ingress

Schema 8 adds `ingress_proxy`, `ingress.register`, and `ingress.handle`. A
plugin declaring ingress must return schema 8 or later; registration rejects
older declarations. Existing schema-7 provider binaries remain supported.

The plugin declares bounded method/prefix/origin rules, then maps request
metadata to a data-only proxy plan. Concrete server routes take precedence.
The host requires a successful frontend authentication result, validates the
plan, selects credentials from the shared active pool, and streams original
request/response bodies with cancellation. Stored credentials never appear in
the ingress RPC. Redirects and protocol upgrades are not supported. Native
plugins remain trusted in-process code, not a security sandbox.

Credential selection does not impose a per-caller account ACL: authenticated
callers share the configured pool. Deployments requiring account-specific
authorization must not treat this capability as providing it. The precise
route, selector, and injection types are documented in `sdk/pluginapi`.

### Handler routes

Schema 9 adds opt-in handler routes for plugins that must read the request body,
for example to route by a JSON `model` field with plugin-configured upstream
keys. A handler route declares an exact `path` and `max_body_bytes` (at most
64 MiB) instead of `path_prefix` and `upstream_origins`. Plugins declaring one
must return schema 9 or later; older hosts reject schema 9 at registration.
Reserved-prefix, method and overlap checks match proxy routes, and concrete
server routes still take precedence.

The host authenticates the frontend request exactly as for proxy routes, then
buffers at most `max_body_bytes` (413 otherwise) and passes it as `body` in
`ingress.handle`. Sensitive header values remain presence-only, so frontend
keys never reach the plugin. The request carries `host_callback_id`; host HTTP
callbacks made with it are cancelled with the inbound request. The plugin
returns `response` (`status_code`, `headers`, `body`) instead of a `plan`. The
host writes it without HTML escaping or other body rewriting, dropping only
hop-by-hop headers and `Content-Length`. Handler routes do not stream.

## HTTP TLS curve profiles

Schema 8 adds `wire_profile.tls_curves` to host HTTP requests. The optional
array accepts `X25519`, `P-256`, `P-384`, and `P-521`; unknown or duplicate
values reject the request before dialing. It defines the exact offered set;
Go applies its internal preference order. The host uses a request-private TLS
configuration without changing HTTP/2, ALPN, or proxy selection. Transports
with a custom TLS dialer are rejected when the host cannot guarantee that the
dialer will honor the profile.

## Native executor request preparation

Before invoking a native executor, the host translates both the current and
original request into the executor input format, applies canonical thinking
validation and suffix handling, then applies configured payload defaults,
overrides, and filters. Payload processing receives the same resolved model,
client-requested alias, source format, headers, and request path used by
built-in executors. Preparation errors are returned without invoking the
plugin callback.

## Native shutdown and image lifetime

Explicit unload and host shutdown call `plugin.quiesce` before retiring host
callback dispatch. Async plugins must stop admission, cancel upstream work, and
wait for their producer goroutines before returning from quiesce. Caller context
cancellation may detach the runtime immediately, but physical cleanup continues
without canceling the quiesce/drain sequence. A hung trusted plugin can therefore
retain resources until process exit; the host must not free callbacks underneath it.

Unix CGO, like the Windows and purego loaders, retains native images until process
exit. Go c-shared runtime threads cannot safely be stopped with `dlclose`, even
after provider work drains. Replacing a native image requires process restart;
logical unload is not a promise to reclaim its executable mappings.

## Build hosts for plugin smoke qualification

Install mise and a C compiler, then run `mise install` to use the pinned patched
Go toolchain. Build separate executables so the plugin repository can exercise
both loaders against the same exact host source commit:

```bash
OUTPUT=/absolute/path/cpa-cgo mise run build
OUTPUT=/absolute/path/cpa-purego mise run build:purego
mise run verify:compile
mise run verify:test
mise run verify:race
mise run verify:purego
```

The plugin repository's `mise run smoke` consumes these paths as `HOST_CGO` and
`HOST_PUREGO`. Host verification does not publish an application release or
deployment. Plugin artifacts require their separate versioned publication gate.
