# k8shelld

[![Build](https://github.com/k8shell-io/k8shelld/actions/workflows/build.yaml/badge.svg)](https://github.com/k8shell-io/k8shelld/actions/workflows/build.yaml)

The k8Shell workspace daemon. Runs as PID 1 inside a k8Shell workspace container, manages session orchestration (`shell`, `exec`, `sftp`, `port-forward`), executes init scripts, supervises blueprint-defined apps, monitors system resources, and exposes gRPC and REST APIs to the rest of the k8Shell platform.

This repository also contains **`kbox`** — a CLI utility that runs inside the container and talks to `k8shelld` over a Unix socket to give users local-like control over their workspace.

## Concepts

### Blueprint

The blueprint is a YAML document mounted at `/etc/k8shell/blueprint.yaml`. It is provided into the workspace by the k8Shell provisioner service before `k8shelld` starts — `k8shelld` does not create or modify it. On startup, `k8shelld` reads the blueprint to determine:

- Whether Podman-in-container is enabled (and whether to create a `/var/run/docker.sock` symlink)
- Which apps are available and which start automatically
- Workspace splash configuration

### Init scripts

On startup, `k8shelld` runs all executable files matching the pattern `__init_*` found in `/usr/local/k8shell/system`, sorted alphabetically. Scripts are executed sequentially in the background so they do not block the gRPC server from accepting connections. Each script runs as the workspace user inside its own process group.

A flag file is written to `~/.k8shell/flags/<script-name>` after a successful run, so scripts are skipped on subsequent container restarts unless the flag is removed. A blueprint init script with `always: true` skips this run-once guard and executes on every start; its flag file is still maintained so the script can tell the two cases apart.

Every init script is run with `K8SHELL_INIT_FIRST_RUN` in its environment — `true` when the script has not previously completed successfully in this workspace, `false` when it is being re-run (only `always` scripts are ever re-run).

Init-script progress is tracked in memory and streamed to the PTY display when a new shell session is opened before the scripts finish.

### App manager

Apps are defined in the blueprint under `apps`. Each app is a supervised process managed by `k8shelld`. The app manager:

- Installs the app on demand from `/usr/local/k8shell/apps`
- Starts and stops apps via the gRPC `AppService`
- Auto-starts apps with `autoStart: true` after all init scripts complete
- Supervises running apps and restarts them on unexpected exit

### Session streams

`k8shelld` manages four types of multiplexed streams over its gRPC connection:

| Type | Description |
|---|---|
| `shell` | Interactive PTY shell. Supports detach/attach when `shells.allowSessionDetach` is enabled. Detached sessions are garbage-collected after `shells.detachedTTL`. |
| `exec` | Non-interactive command execution with streamed stdout/stderr. |
| `sftp` | File transfer session backed by a standalone `sftp` sub-process. |
| `port-forward` | TCP port-forward to a destination reachable from within the pod. |

All stream lifecycle events are tracked in in-memory stores. Completed entries are pruned after a short delay so recent history remains queryable.

### JWT authentication

Every inbound gRPC call is authenticated by a unary interceptor that verifies a JWT passed in the `token` metadata key. The token must be signed by the k8Shell identity service and must match the workspace token loaded at startup. Calls without a valid token are rejected with `PermissionDenied`.

### Process watcher

`k8shelld` runs a process watcher that:

- Reaps zombie (defunct) child processes so they do not accumulate under PID 1
- Optionally terminates orphan processes (processes whose parent is PID 1 but were not started by `k8shelld`)

Both behaviours are controlled by `terminateOrphans` and `reapZombies` in the daemon configuration.

### System info

`k8shelld` collects CPU and memory usage for the workspace container every 30 seconds using cgroups. The collected stats are served via the `SystemService` gRPC endpoint and exposed through the REST API.

### REST API

A lightweight HTTP API is exposed over a Unix domain socket. It is consumed by `kbox` and is not accessible from outside the container. The REST API provides access to stream state, system info, app status, init-script progress, and workspace identity.

### gRPC API

`k8shelld` exposes its gRPC interface defined in the `github.com/k8shell-io/common` module:

- `SystemService` — workspace info, active stream listing, system resource stats
- `SshService` — `shell`, `exec`, `sftp`, and `port-forward` stream lifecycle
- `AppService` — app install, start, stop, and status
- `CommandService` — fire-and-forget command execution

## Repository layout

```
cmd/
  k8shelld/    # Daemon entry point and config loading
  kbox/        # CLI companion entry point and subcommands
sftp/          # Standalone sftp server binary (launched as a subprocess)
internal/
  apps/        # App manager and supervisor
  config/      # Config and blueprint loading
  display/     # Init-progress display for PTY sessions
  grpc/        # gRPC server, stream handlers, session store
  logger/      # Zerolog wrapper
  models/      # Shared types (user, init tracker, stream records)
  server/      # Top-level server: init scripts, REST API, signal handling
  system/      # Process watcher, user management, cgroups stats
  utils/       # Misc helpers
scripts/       # Init scripts bundled into the image
docker/
  k8shelld/    # Dockerfile and BUILD file
tests/         # Integration smoke-test scripts and config
```

## Prerequisites

- Go 1.24+
- Docker

## Local development

**Build all binaries:**
```bash
make build
```

**Run `k8shelld`** (requires a running Kubernetes pod environment or a suitable test config):
```bash
./bin/k8shelld --config tests/config.yaml
```

**Use `kbox`** to interact with a running daemon:
```bash
./bin/kbox info
```

## Makefile targets

| Target | Description |
|---|---|
| `make build` | Compile `k8shelld`, `kbox`, and `sftp` binaries to `bin/` |
| `make test` | Run unit tests with coverage |
| `make test-static` | Run `golangci-lint` and `gosec` |
| `make test-self` | Static analysis + build + binary smoke tests (used in CI) |
| `make image` | Build Docker image (Alpine debug by default) |
| `make image-debug` | Shorthand for `RUNTIME=alpine make image` |
| `make image-release` | Build stripped release image (`RUNTIME=release`) |
| `make vendor` | Vendor Go dependencies |

## Docker images

Two runtime stages are available in `docker/k8shelld/Dockerfile`:

| Stage | Base | Use case |
|---|---|---|
| `alpine` | `alpine:3.21.3` | Development and debugging (has a shell, debug symbols retained) |
| `release` | `alpine:3.21.3` | Production (stripped binary, `-ldflags="-s -w"`) |

Both stages copy the same three binaries — `k8shelld`, `kbox`, and `sftp` — to `/k8shell/dist/`, along with the `scripts/` directory.

## Running in Kubernetes

Deployment configuration and Helm charts for k8shelld and the other k8Shell services are maintained in the [k8shell-io/charts](https://github.com/k8shell-io/charts) repository.

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
