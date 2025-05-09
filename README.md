## k8shelld

**k8shelld** is the init process running as PID 1 within the workspace container.

It provides the following core capabilities:

- **gRPC API services**: Maps to SSH protocol channels, including: shell with PTY and agent forwarding, exec, port forwarding, sFTP subsystem

- **Process management**. Reaps zombie and orphaned processes.

- **`kbox` CLI**. A companion command-line tool that allows the workspace to interact with `k8shelld` via a Unix socket.

- **Resource monitoring**. Collects CPU and memory usage statistics directly from the container’s cgroup v2 interface.
