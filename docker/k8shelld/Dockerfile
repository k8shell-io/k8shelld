# syntax=docker/dockerfile:1
# Copyright 2025 the k8Shell authors.
# SPDX-License-Identifier: AGPL-3.0-or-later

# -- Build stage (debug) -------------------------------------------------------
FROM registry.k8shell.io/docker/library/golang:1.24.5 AS build-debug

ARG VERSION
ARG COMMIT_ID

LABEL VERSION=$VERSION
LABEL COMMIT_ID=$COMMIT_ID

WORKDIR /go/src/k8shelld

COPY files .
ENV GOFLAGS="-mod=vendor"

RUN CGO_ENABLED=0 go build -gcflags="all=-N -l" \
    -ldflags="-X github.com/k8shell-io/k8shelld/internal/config.K8SHELLD_VERSION=$VERSION -X github.com/k8shell-io/k8shelld/internal/config.K8SHELLD_COMMIT=$COMMIT_ID" \
    -o /go/bin/k8shelld ./cmd/k8shelld

RUN CGO_ENABLED=0 go build -gcflags="all=-N -l" \
    -ldflags="-X main.version=$VERSION -X main.commit_id=$COMMIT_ID" \
    -o /go/bin/kbox ./cmd/kbox

RUN CGO_ENABLED=0 go build -gcflags="all=-N -l" \
    -o /go/bin/sftp ./sftp

# -- Build stage (release) -----------------------------------------------------
FROM registry.k8shell.io/docker/library/golang:1.24.5 AS build-release

ARG VERSION
ARG COMMIT_ID

WORKDIR /go/src/k8shelld

COPY files .
ENV GOFLAGS="-mod=vendor"

RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/k8shell-io/k8shelld/internal/config.K8SHELLD_VERSION=$VERSION -X github.com/k8shell-io/k8shelld/internal/config.K8SHELLD_COMMIT=$COMMIT_ID" \
    -o /go/bin/k8shelld ./cmd/k8shelld

RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.version=$VERSION -X main.commit_id=$COMMIT_ID" \
    -o /go/bin/kbox ./cmd/kbox

RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w" \
    -o /go/bin/sftp ./sftp

# -- Runtime stage (Alpine / debug) --------------------------------------------
FROM registry.k8shell.io/docker/library/alpine:3.21.3 AS alpine

COPY --from=build-debug /go/bin/k8shelld /k8shell/dist/
COPY --from=build-debug /go/bin/kbox /k8shell/dist/
COPY --from=build-debug /go/bin/sftp /k8shell/dist/
COPY files/scripts /k8shell/dist/scripts

# -- Runtime stage (Alpine / release) ------------------------------------------
FROM registry.k8shell.io/docker/library/alpine:3.21.3

COPY --from=build-release /go/bin/k8shelld /k8shell/dist/
COPY --from=build-release /go/bin/kbox /k8shell/dist/
COPY --from=build-release /go/bin/sftp /k8shell/dist/
COPY files/scripts /k8shell/dist/scripts