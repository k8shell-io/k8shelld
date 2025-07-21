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

image:
	@echo "Building k8shelld docker image..."
	@rm -fr docker/files
	@mkdir -p docker/files
	version=$$(git describe --tags --match '*' | cut -d'-' -f1-2) && \
	echo -n "k8shell-base/k8shelld:$$version" > docker/BUILD && \
	cp -r go.mod go.sum grpc internal cmd sftp scripts docker/files && \
	cd docker && docker build --build-arg VERSION=$$version \
		--build-arg COMMIT_ID=$$(git rev-parse --short HEAD) -t $(REPO)/$$(cat ./BUILD) .
	#cd docker && docker push $(REPO)/$$(cat ./BUILD)

protoc:
	echo "Generating Go code from proto file..."
	cd grpc && \
	rm -fr generated-go && \
	protoc --go_out=. --go-grpc_out=. --go_opt=Mk8shelld.proto=generated-go/k8shelldpb --go-grpc_opt=Mk8shelld.proto=generated-go/k8shelldpb   k8shelld.proto
# 	cd grpc && python \
# 		-m grpc_tools.protoc \
# 		--python_out=../../k8shell-proxy/k8shell_proxy/grpc_generated \
# 		--grpc_python_out=../../k8shell-proxy/k8shell_proxy/grpc_generated \
# 		-I . k8shelld.proto
