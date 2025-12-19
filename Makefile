# Variables
GOOS_LIST := linux
GOARCH_LIST := amd64 arm64
REPO=fitcr.ksi.in.fit.cvut.cz

# Default target
all: build

# Initialize Go module
init:
	@echo "Initializing Go module..."
	go mod tidy

# Run unit tests with coverage
test:
	@echo "Running unit tests..."
	go test ./... -cover

# Build binaries
build:
	@echo "Building k8shelld..."
	go build -o bin/k8shelld ./cmd/k8shelld
	@echo "Building kbox..."
	go build -o bin/kbox ./cmd/kbox
	@echo "Build complete!"

# Run binary smoke tests
test-binary: build
	@echo "Running binary smoke tests..."
	@./bin/kbox -h > /dev/null 2>&1 || (echo "kbox help failed" && exit 1)
	@./bin/k8shelld -h > /dev/null 2>&1 || (echo "k8shelld help failed" && exit 1)
	@echo "Binary smoke tests passed!"

image:
	@echo "k8shelld docker image"
	@rm -fr docker/k8shelld/files
	@mkdir -p docker/k8shelld/files
	@echo "Downloading vendor modules..."
	@go mod vendor -o docker/k8shelld/files/vendor
	@echo "Building image..."
	@version=$$(git describe --tags --match 'v*' | sed 's/-g.*//') && \
	echo -n "k8shell-test/k8shelld:$$version" > docker/k8shelld/BUILD && \
	cp -r go.mod go.sum internal pkg cmd sftp scripts docker/k8shelld/files && \
	cd docker/k8shelld && docker build --build-arg VERSION=$$version \
		--build-arg COMMIT_ID=$$(git rev-parse --short HEAD) -t $(REPO)/$$(cat ./BUILD) .

protoc:
	@echo "Generating Go code from proto file..."
	rm -rf pkg/api/k8shelldpb
	protoc \
		--go_out=module=github.com/k8shell-io/k8shelld:. \
		--go-grpc_out=module=github.com/k8shell-io/k8shelld:. \
		pkg/api/k8shelld.proto
