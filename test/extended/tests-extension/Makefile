# Get the directory where this Makefile is, so we can use it below for including
DIR := $(strip $(shell dirname $(realpath $(lastword $(MAKEFILE_LIST)))))

# Definitions for the extended tests
GO_PKG_NAME := github.com/openshift-eng/openshift-tests-extension

GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo 'unknown')
BUILD_DATE := $(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
GIT_TREE_STATE := $(shell if git rev-parse --git-dir > /dev/null 2>&1; then if git diff --quiet; then echo clean; else echo dirty; fi; else echo unknown; fi)

LDFLAGS := -X '$(GO_PKG_NAME)/pkg/version.CommitFromGit=$(GIT_COMMIT)' \
           -X '$(GO_PKG_NAME)/pkg/version.BuildDate=$(BUILD_DATE)' \
           -X '$(GO_PKG_NAME)/pkg/version.GitTreeState=$(GIT_TREE_STATE)'

METADATA := $(shell pwd)/.openshift-tests-extension

TOOLS_BIN_DIR := $(CURDIR)/bin
BINARY_NAME := cluster-etcd-operator-tests-ext

.PHONY: help
help: #HELP Display essential help.
	@awk 'BEGIN {FS = ":[^#]*#HELP"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\n"} /^[a-zA-Z_0-9-]+:.*#HELP / { printf "  \033[36m%-17s\033[0m %s\n", $$1, $$2 } ' $(MAKEFILE_LIST)

#SECTION Development
.PHONY: verify #HELP To verify the code
verify: tidy fmt vet

.PHONY: tidy #HELP To tidy the go.mod and go.sum files
tidy:
	go mod tidy

.PHONY: fmt #HELP To run go fmt
fmt:
	go fmt ./...

.PHONY: vet #HELP To run go vet
vet:
	go vet ./...

#SECTION Build
.PHONY: build
build: #HELP Build the extended tests binary
	@mkdir -p $(TOOLS_BIN_DIR)
	GO_COMPLIANCE_POLICY="exempt_all" CGO_ENABLED=0 go build -mod=mod -ldflags "$(LDFLAGS)" -o $(TOOLS_BIN_DIR)/$(BINARY_NAME) ./cmd/...

.PHONY: update-metadata
update-metadata: build #HELP Build and run 'update-metadata' to generate test metadata
	$(TOOLS_BIN_DIR)/$(BINARY_NAME) update --output-dir=$(METADATA)

.PHONY: build-update
build-update: build #HELP Build and update test metadata (strips machine-specific codeLocations)
	$(TOOLS_BIN_DIR)/$(BINARY_NAME) update --output-dir=$(METADATA)
	for f in $(METADATA)/*.json; do \
		jq 'map(del(.codeLocations))' "$$f" > tmpp && mv tmpp "$$f"; \
	done

.PHONY: clean
clean: #HELP Remove the bin directory
	rm -rf $(TOOLS_BIN_DIR)

.PHONY: run-suite
run-suite: build #HELP Run a test suite (usage: make run-suite SUITE=<suite-name> [JUNIT_DIR=<dir>])
	@if [ -z "$(SUITE)" ]; then \
		echo "Error: SUITE variable is required. Usage: make run-suite SUITE=<suite-name> [JUNIT_DIR=<dir>]"; \
		exit 1; \
	fi
	@JUNIT_ARG=""; \
	if [ -n "$(JUNIT_DIR)" ]; then \
		mkdir -p $(JUNIT_DIR); \
		JUNIT_ARG="--junit-path=$(JUNIT_DIR)/junit.xml"; \
	fi; \
	$(TOOLS_BIN_DIR)/$(BINARY_NAME) run-suite $(SUITE) $$JUNIT_ARG

.PHONY: list-test-names
list-test-names: build #HELP List all test names
	$(TOOLS_BIN_DIR)/$(BINARY_NAME) list

.PHONY: verify-metadata
verify-metadata: build #HELP Verify metadata is up to date
	@echo "Verifying metadata is up to date..."
	@$(TOOLS_BIN_DIR)/$(BINARY_NAME) update --output-dir=$(METADATA) --dry-run || \
		(echo "Error: metadata is not up to date. Run 'make build-update' to update it."; exit 1)
	@echo "Metadata is up to date."
