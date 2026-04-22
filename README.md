# k8shelld 

[![Self-Tests](https://github.com/k8shell-io/k8shelld/actions/workflows/self-tests.yaml/badge.svg)](https://github.com/k8shell-io/k8shelld/actions/workflows/self-tests.yaml)

**K8shelld** is init process for the k8shell workspace. It is a a secure, container-native development environment framework built on top of Kubernetes. It provides remote shell access, development tooling, and runtime control within isolated Kubernetes pods—while preserving compatibility with standard developer workflows.

This repository contains the following core components:

- **`k8shelld`** – the workspace init process (PID 1) responsible for handling gRPC and REST APIs, session orchestration (`shell`, `exec`, `sftp`, `port-forward`), and system monitoring.
- **`kbox`** – a CLI utility running inside the container that interacts with `k8shelld` through a Unix socket, giving users local-like control over the workspace.

## Features

- SSH-based access via gRPC channel multiplexing
- Support for `shell`, `exec`, `sftp`, `port-forward` streams
- Built-in PTY and agent-forwarding support
- Docker-in-Docker support for isolated container builds
- Lightweight, self-contained init process (`k8shelld`)
- `kbox` CLI for interacting with the workspace runtime
- cgroups-based CPU and memory usage stats
- API server and SSH proxy integration


