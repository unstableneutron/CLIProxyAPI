# AGENTS.md

Go 1.26+ proxy server providing OpenAI/Gemini/Claude/Codex compatible APIs with OAuth and round-robin load balancing.

## Repository
- GitHub: https://github.com/router-for-me/CLIProxyAPI

## Commands
```bash
gofmt -w . # Format (required after Go changes)
go build -o cli-proxy-api ./cmd/server # Build
go run ./cmd/server # Run dev server
go test ./... # Run all tests
go test -v -run TestName ./path/to/pkg # Run single test
go build -o test-output ./cmd/server && rm test-output # Verify compile (REQUIRED after changes)
```
- Common flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`

## Fork Ship and release boundary

- Keep the fork as a discrete feature set. Prefer upstream implementations when
  they satisfy the same contract, retaining regression tests that prove parity.
  Keep changes concise and surgical: place fork-specific helpers in adjacent
  feature files and touch upstream files only at necessary call sites when that
  reduces merge overlap. Follow the executor `helps/` boundary below. Do not
  move code merely to rename it or remove a safeguard without equivalent tests.
- In `unstableneutron/CLIProxyAPI`, Ship defaults to a normal source push only.
  Merge upstream into published main; never rebase or force-push published history.
  Record origin and upstream commits, run `mise run verify`, commit, then rerun
  gates on the exact clean commit. Fetch origin immediately before pushing and
  stop if its main changed. Never deploy services as part of Ship.
- For plugin-host changes, use `HOST_DIR=/absolute/output ARCH=amd64 mise run build:smoke`
  from clean source. For Linux arm64 set `ARCH=arm64 CC=aarch64-linux-gnu-gcc`.
  The task builds both loader modes; cpa-plugins owns metadata validation,
  architecture-aware packaging and extracted-library smoke. Follow that repo's
  README release boundary, including compatibility checks of shipped assets.
- CPA tags trigger host archives AND GHCR publication. Source authorization is
  not tag/release authorization. Use `mise run release:plan`, then explicitly
  `mise run release:publish <computed-tag>`; never manually guess release tags.
  Tags are `v<upstream-version>-un.<build>` prereleases, never stable/latest.
  The positive integer build starts at 1 independently for each exact upstream
  release at the source/upstream-main merge-base. Historical `v7.2.94-un.0.1.*`
  tags are preserved but excluded from numbering. Ambiguous bases, malformed
  tags, unpushed/dirty source and origin drift must stop publication.
  Run `mise run release:build` on pushed source, download that run's archives,
  and qualify the exact platform/loader artifacts before tagging. Record tests,
  smoke, ABI/schema requirements and platform limitations; BUILDINFO records
  source/toolchain/target metadata. Set `QUALIFIED_RUN=<run-id>` and
  `QUALIFIED_DIR=<directory-with-only-the-14-archives>` for release:publish.
  The tag binds the run and checksums; Actions publishes those same archives,
  then downloads and compares them. Independently verify downloads and smoke
  applicable Linux binaries with published plugins; do not claim unsupported
  platforms were runtime-tested. Source Ship alone is not a vetted release.
- CPA never publishes cpa-plugins automatically. Plugin publication policy is
  canonical in that repository's README; host-only changes do not authorize it.
  Never log credentials or include auth files in build metadata.

## Config
- Default config: `config.yaml` (template: `config.example.yaml`)
- `.env` is auto-loaded from the working directory
- Auth material defaults under `auths/`
- Storage backends: file-based default; optional Postgres/git/object store (`PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`)

## Architecture
- `cmd/server/` — Server entrypoint
- `internal/api/` — Gin HTTP API (routes, middleware, modules)
- `internal/api/modules/amp/` — Amp integration (Amp-style routes + reverse proxy)
- `internal/thinking/` — Main thinking/reasoning pipeline. `ApplyThinking()` (apply.go) parses suffixes (`suffix.go`, suffix overrides body), normalizes config to canonical `ThinkingConfig` (`types.go`), normalizes and validates centrally (`validate.go`/`convert.go`), then applies provider-specific output via `ProviderApplier`. Do not break this "canonical representation → per-provider translation" architecture.
- `internal/runtime/executor/` — Per-provider runtime executors (incl. Codex WebSocket)
- `internal/translator/` — Provider protocol translators (and shared `common`)
- `internal/registry/` — Model registry + remote updater (`StartModelsUpdater`); `--local-model` disables remote updates
- `internal/store/` — Storage implementations and secret resolution
- `internal/managementasset/` — Config snapshots and management assets
- `internal/cache/` — Request signature caching
- `internal/watcher/` — Config hot-reload and watchers
- `internal/wsrelay/` — WebSocket relay sessions
- `internal/usage/` — Usage and token accounting
- `internal/home/` — CLIProxyAPIHome control plane integration (bootstrap, RESP communication, dispatch coordination)
- `internal/tui/` — Bubbletea terminal UI (`--tui`, `--standalone`)
- `sdk/cliproxy/` — Embeddable SDK entry (service/builder/watchers/pipeline)
- `test/` — Cross-module integration tests

## Code Conventions
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific (e.g. `README_CN.md`)
- As a rule, do not make standalone changes to `internal/translator/`. You may modify it only as part of broader changes elsewhere.
- If a task requires changing only `internal/translator/`, run `gh repo view --json viewerPermission -q .viewerPermission` to confirm you have `WRITE`, `MAINTAIN`, or `ADMIN`. If you do, you may proceed; otherwise, file a GitHub issue including the goal, rationale, and the intended implementation code, then stop further work.
- `internal/runtime/executor/` should contain executors and their unit tests only. Place any helper/supporting files under `internal/runtime/executor/helps/`.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, and the `cmd/fetch_antigravity_models` utility timeouts
- Avoid wall-clock `time.Sleep` in TTL, expiration, ordering, or cache-eviction unit tests due to platform timer granularity (e.g. Windows default timer resolution of ~15.6ms) and CI jitter under load; prefer controllable clocks (`nowFunc` / mock clock), explicit timestamp manipulation, or deterministic synchronization primitives.
- Note: if modifying features that involve CLIProxyAPIHome, check if corresponding updates are needed in the CLIProxyAPIHome repository.
