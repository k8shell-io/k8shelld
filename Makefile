# Variables
GOOS_LIST := linux
GOARCH_LIST := amd64 arm64
REPO=fitcr.ksi.in.fit.cvut.cz

# Default target
all: build

# Initialize Go module
# Ensures go.mod and go.sum are up to date with dependencies
init:
	@echo "Initializing Go module..."
	go mod tidy

# Run unit tests with coverage
# Used in CI/CD workflow to validate code changes before building
test:
	@echo "Running unit tests..."
	go test ./... -cover

# Build binaries
# Compiles k8shelld and kbox executables for local development and CI/CD validation
build:
	@echo "Building k8shelld..."
	go build -o bin/k8shelld ./cmd/k8shelld
	@echo "Building kbox..."
	go build -o bin/kbox ./cmd/kbox
	@echo "Build complete!"

# Run binary smoke tests
# Validates that built binaries execute successfully (basic sanity check)
# Used in CI/CD workflow after build step
test-binary: build
	@echo "Running binary smoke tests..."
	@./bin/kbox -h > /dev/null 2>&1 || (echo "kbox help failed" && exit 1)
	@./bin/k8shelld -h > /dev/null 2>&1 || (echo "k8shelld help failed" && exit 1)
	@echo "Binary smoke tests passed!"

# Vendor Go modules
# Downloads and stores dependencies locally for reproducible Docker builds
# Used in CI/CD workflow before preparing Docker context
vendor:
	@echo "Vendoring Go modules..."
	@go mod vendor
	@echo "Vendoring complete!"

# Prepare Docker build context
# Copies vendored dependencies and source files to docker/k8shelld/files/
# Used in CI/CD workflow before building container image
prepare-docker:
	@echo "Preparing Docker build context..."
	@rm -rf docker/k8shelld/files
	@mkdir -p docker/k8shelld/files
	@cp -r vendor docker/k8shelld/files/
	@cp -r go.mod go.sum internal pkg cmd sftp scripts docker/k8shelld/files/
	@echo "Docker context prepared!"

# Build Docker image
# Builds k8shelld container image with version tagging
# Accepts VERSION, COMMIT_ID, IMAGE_TAG from environment or auto-detects from git
# Can be used locally or in CI/CD workflow
image: vendor prepare-docker
	@echo "Building k8shelld docker image..."
	@VERSION=$${VERSION:-$$(git describe --tags --match 'v*' | sed 's/-g.*//')} && \
	COMMIT_ID=$${COMMIT_ID:-$$(git rev-parse --short HEAD)} && \
	IMAGE_TAG=$${IMAGE_TAG:-$$VERSION} && \
	echo -n "k8shell-test/k8shelld:$$IMAGE_TAG" > docker/k8shelld/BUILD && \
	cd docker/k8shelld && docker build --build-arg VERSION=$$VERSION \
		--build-arg COMMIT_ID=$$COMMIT_ID -t $(REPO)/$$(cat ./BUILD) .

# Generate gRPC code from protobuf definitions
# Regenerates Go code from k8shelld.proto file when API changes
protoc:
	@echo "Generating Go code from proto file..."
	rm -rf pkg/api/k8shelldpb
	protoc \
		--go_out=module=github.com/k8shell-io/k8shelld:. \
		--go-grpc_out=module=github.com/k8shell-io/k8shelld:. \
		pkg/api/k8shelld.proto
