# Native plugin failure contract

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
