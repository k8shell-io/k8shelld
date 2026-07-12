# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make build          # compile k8shelld, kbox, sftp to bin/
make test           # unit tests with coverage
make test-static    # golangci-lint + gosec
make test-self      # static + build + smoke tests (what CI runs)
make vendor         # vendor dependencies
make image          # build Alpine (debug) image
make image-release  # build release image (stripped binary)
```

Run a single test:
```bash
go test ./internal/utils/... -run TestFunctionName -v
```

## Architecture

`k8shelld` is the PID 1 init process for a k8Shell workspace container. It owns the full container lifecycle: user provisioning, init scripts, session streams, app supervision, and graceful shutdown on SIGTERM/SIGINT.

**Startup sequence** (`internal/server/server.go`):
1. Load config YAML + blueprint from `/etc/k8shell/blueprint.yaml` (blueprint is placed there by the k8Shell provisioner — `k8shelld` never writes it)
2. Load identity from env vars `USER_UID`, `USER_GID`, etc.
3. Create the workspace OS user (`internal/system/users.go`) — dispatches to an Alpine or standard provider depending on which tools are present
4. Run init scripts from `/usr/local/k8shell/system/__init_*` sequentially in the background; progress is tracked in `models.InitTracker` and streamed to new PTY sessions via `internal/display/`
5. Auto-start blueprint apps after all init scripts complete
6. Serve gRPC + REST APIs concurrently

**Key packages:**

- `internal/server` — top-level `Server` struct, orchestrates everything above; also contains identity loading from env vars (`identity.go`), REST API (`restapi.go`), credential helper setup (`credhelpers.go`), and tool wrappers (`toolswrapper.go`)
- `internal/grpc` — gRPC server (`grpcapi.go`) with a JWT unary interceptor; stream handlers for `shell`, `exec`, `sftp`, `port-forward`, `unix-socket`; in-memory stores (`sync.Map`) per stream type; detachable PTY session GC
- `internal/apps` — `AppManager` installs and supervises blueprint-defined apps from `/usr/local/k8shell/apps`; `AppSupervisor` restarts apps on unexpected exit
- `internal/system` — `ProcessWatcher` reaps zombies and terminates orphans; `SystemInfo` collects cgroups CPU/memory every 30 s; `users.go` dispatches user creation to `users_alpine.go` or `users_standard.go`
- `internal/models` — `User` (RW-locked, holds workspace identity claims), `ShellUser` (immutable snapshot for session lifetime), `InitTracker` (per-script state machine)
- `internal/apiclient` — wraps the `k8shell-go` SDK client, authenticated with the static PAT from `K8SHELL_PAT_TOKEN` for outbound calls to the API server (`apiServer.enabled: true`)
- `cmd/kbox` — CLI companion; communicates with `k8shelld` exclusively over the Unix socket REST API (never gRPC directly)
- `sftp/` — standalone binary launched as a subprocess by the sftp stream handler

**gRPC API** is defined in `github.com/k8shell-io/common` (external module). The four registered services are `SystemService`, `SshService`, `AppService`, and `CommandService`. All calls require a JWT in the `token` gRPC metadata key that must match the workspace token.

**REST API** is a Unix-socket HTTP server (`internal/server/restapi.go`). It is only accessible inside the container and is the sole transport used by `kbox`.

**Identity lifecycle**: workspace identity (username, UID, GID, display name, email) is loaded once at startup from env vars — there is no token-issuance endpoint call and no renewal. Outbound calls to the API server (`internal/apiclient`) authenticate separately with a static PAT read from `K8SHELL_PAT_TOKEN`.

**Build produces three binaries**: `k8shelld`, `kbox`, `sftp` — all `CGO_ENABLED=0`. The Dockerfile has two runtime stages (`alpine` for debug, `release` for production) on top of two build stages.

## Environment variables

| Variable | Purpose |
|---|---|
| `USERNAME` | Workspace OS username (required) |
| `WORKSPACE` | Workspace name (required) |
| `JWT_VERIFIER_SIGNING_METHOD` | `rs256` or `hs256` |
| `JWT_VERIFIER_PUBLIC_KEY` | Base64-encoded public key (rs256) or secret (hs256) |
| `USER_UID` / `USER_GID` | Workspace user's UID/GID (required) |
| `USERFULLNAME` / `USEREMAIL` | Optional display name / email fallback |
| `K8SHELL_PAT_TOKEN` | Personal access token for outbound API server calls; required when `apiServer.enabled: true` |
