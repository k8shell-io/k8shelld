# Variables
REPO=fitcr.ksi.in.fit.cvut.cz
REPORTS_DIR := reports
VENV := .venv

# Default target
all: build

init:  ##@ Initialize Go module
       ##@ Ensures go.mod and go.sum are up to date with dependencies
	@echo "Initializing Go module..."
	go mod tidy
	go mod download

install-test-deps: ##@ Install test dependencies
                   ##@ Installs golangci-lint and gosec for static analysis
	@echo "Installing test dependencies..."
	@echo "Installing golangci-lint..."
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8
	@echo "Installing gosec..."
	go install github.com/securego/gosec/v2/cmd/gosec@v2.22.11
	@echo "Installing go-junit-report..."
	go install github.com/jstemmer/go-junit-report/v2@v2.1.0
	@mkdir -p $(REPORTS_DIR)

test-static: ##@ Run static analysis
             ##@ Runs linting and security checks on Go code
             ##@ Used in CI/CD workflow to catch code quality and security issues
test-static: install-test-deps
	@echo "Running golangci-lint..."
	golangci-lint run ./...
	@echo "Running gosec security scan for HIGH severity issues only..."
	##gosec -fmt=junit-xml -out=$(REPORTS_DIR)/gosec-junit.xml -severity low -quiet ./...
	rm -f $(REPORTS_DIR)/gosec.sarif
	gosec -fmt=sarif -out=$(REPORTS_DIR)/gosec.sarif -severity medium -quiet ./...
	@if [ ! -s $(REPORTS_DIR)/gosec-junit.xml ]; then echo '<?xml version="1.0" encoding="UTF-8"?><testsuites></testsuites>' > $(REPORTS_DIR)/gosec-junit.xml; fi
	@echo "Static analysis passed!"

test:       ##@ Run unit tests with coverage
            ##@ Validates code correctness through unit tests
            ##@ -count=1 disables test caching to ensure fresh execution in CI/CD
test: install-test-deps
	@echo "Running unit tests..."
	go test ./... -cover -coverprofile=$(REPORTS_DIR)/coverage.out -count=1 -v 2>&1 | go-junit-report -set-exit-code > $(REPORTS_DIR)/unit-junit.xml
	@echo "Unit tests passed!"

build:      ##@ Build k8shelld and kbox binaries
	@echo "Building k8shelld..."
	go build -o bin/k8shelld ./cmd/k8shelld
	@echo "Building kbox..."
	go build -o bin/kbox ./cmd/kbox
	@echo "Build complete!"

test-binary: ##@ Run binary smoke tests
             ##@ Validates that built binaries execute successfully (basic sanity check)
             ##@ Used in CI/CD workflow after build step
test-binary: build
	@echo "Running binary smoke tests..."
	@./bin/kbox -h > /dev/null 2>&1 || (echo "kbox help failed" && exit 1)
	@echo "kbox smoke tests passed!"
	@./bin/k8shelld -h > /dev/null 2>&1 || (echo "k8shelld help failed" && exit 1)
	@echo "k8shelld smoke tests passed!"

test-self:  ##@ Run all self-tests
            ##@ Executes static analysis, unit tests, build, and binary smoke tests
            ##@ Comprehensive validation of code quality and functionality (ran by self-tests CI workflow)
test-self: test-static test build test-binary
	@echo "All self-tests passed!"

vendor:  ##@ Vendor Go modules
         ##@ Downloads and vendors all Go module dependencies into the vendor/ directory
		 ##@ Used in CI/CD workflow before preparing Docker context
	@echo "Vendoring Go modules..."
	@go mod vendor
	@echo "Vendoring complete!"

prepare-docker:  ##@ Prepare Docker build context
                 ##@ Copies vendored dependencies and source files to docker/k8shelld/files/
                ##@ Used in CI/CD workflow before building container image
	@echo "Preparing Docker build context..."
	@rm -rf docker/k8shelld/files
	@mkdir -p docker/k8shelld/files
	@cp -r vendor docker/k8shelld/files/
	@cp -r go.mod go.sum internal pkg cmd sftp scripts docker/k8shelld/files/
	@echo "Docker context prepared!"

image:  ##@ Build Docker image
        ##@ Builds k8shelld container image with version tagging
        ##@ Accepts VERSION, COMMIT_ID, IMAGE_TAG from environment or auto-detects from git
        ##@ Can be used locally or in CI/CD workflow
image: vendor prepare-docker
	@echo "Building k8shelld docker image..."
	@if ! command -v git >/dev/null 2>&1; then echo "Git not found. Please install Git."; exit 1; fi
	@VERSION=$${VERSION:-$$(git describe --tags --match 'v*' | sed 's/-g.*//')} && \
	COMMIT_ID=$${COMMIT_ID:-$$(git rev-parse --short HEAD)} && \
	IMAGE_TAG=$${IMAGE_TAG:-$$VERSION} && \
	echo -n "k8shell-test/k8shelld:$$IMAGE_TAG" > docker/k8shelld/BUILD && \
	cd docker/k8shelld && docker build --build-arg VERSION=$$VERSION \
		--build-arg COMMIT_ID=$$COMMIT_ID -t $(REPO)/$$(cat ./BUILD) .

coverage:  ##@ Calculate test coverage percentage from coverage.out
	@go tool cover -func=$(REPORTS_DIR)/coverage.out | grep total | awk '{print $$3}'

##@
##@ Misc commands
##@

clean: ##@ Clean up generated files
	rm -rf $(REPORTS_DIR)
	rm -f bin/k8shelld
	rm -f bin/kbox
	rm -rf vendor/
	rm -rf docker/k8shelld/files/

clean-all: ##@ Remove all generated files
clean-all: clean

help: ##@ (Default) Print listing of key targets with their descriptions
	@printf "\nUsage: make <command>\n"
	@grep -F -h "##@" $(MAKEFILE_LIST) | grep -F -v grep -F | sed -e 's/\\$$//' | awk 'BEGIN {FS = ":*[[:space:]]*##@[[:space:]]*"}; \
	{ \
		if($$2 == "") \
			printf ""; \
		else if($$0 ~ /^#/) \
			printf "\n%s\n", $$2; \
		else if($$1 == "") \
			printf "     %-20s%s\n", "", $$2; \
		else \
			printf "\n    \033[34m%-20s\033[0m %s\n", $$1, $$2; \
	}'
