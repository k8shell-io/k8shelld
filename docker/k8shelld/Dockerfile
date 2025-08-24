FROM golang:1.24.5 AS build

ARG VERSION
ARG COMMIT_ID

LABEL VERSION=$VERSION
LABEL COMMIT_ID=$COMMIT_ID

WORKDIR /go/src/workspace-tools
COPY files .

RUN go mod download
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X github.com/k8shell-io/k8shelld/internal/server.K8SHELLD_VERSION=$VERSION -X github.com/k8shell-io/k8shelld/internal/server.K8SHELLD_COMMIT=$COMMIT_ID" -o /go/bin/k8shelld ./cmd/k8shelld
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$VERSION -X main.commit_id=$COMMIT_ID" -o /go/bin/kbox ./cmd/kbox
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /go/bin/sftp ./sftp

FROM alpine:3.21.3

COPY --from=build /go/bin/k8shelld /k8shell/dist/
COPY --from=build /go/bin/kbox /k8shell/dist/
COPY --from=build /go/bin/sftp /k8shell/dist/
COPY files/scripts /k8shell/dist/scripts