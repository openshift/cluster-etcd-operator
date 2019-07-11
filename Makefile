ROOT_DIR:=$(shell git rev-parse --show-toplevel)
GOFILES:=$(shell find . -name '*.go' | grep -v -E '(./vendor)')
GOFLAGS='-mod=vendor'
IMAGE_REPO=
IMAGE_TAG:=$(shell $(ROOT_DIR)/hack/git-version.sh)

all: build
.PHONY: all

$(shell mkdir -p bin)

build: bin/cluster-etcd-operator

bin/cluster-etcd-operator: $(GOFILES)
	@go build $(GOFLAGS) -o $(ROOT_DIR)/bin/cluster-etcd-operator github.com/openshift/cluster-etcd-operator/cmd/cluster-etcd-operator

test-unit:
	@go test -v ./...

verify:
	@go vet $(shell go list ./... | grep -v /vendor/)

verify-deps:
	@go mod tidy
	@go mod vendor
	@go mod verify

clean:
	rm -rf $(ROOT_DIR)/bin

.PHONY: build clean verify-deps
