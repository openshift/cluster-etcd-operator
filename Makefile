all: build
.PHONY: all

# Include the library makefile
include $(addprefix ./vendor/github.com/openshift/build-machinery-go/make/, \
	golang.mk \
	targets/openshift/images.mk \
	targets/openshift/deps-gomod.mk \
	targets/openshift/operator/telepresence.mk \
)

IMAGE_REGISTRY :=registry.svc.ci.openshift.org

# OpenShift Tests Extension variables
TESTS_EXT_BINARY ?= cluster-etcd-operator-tests-ext
TESTS_EXT_PACKAGE ?= ./cmd/cluster-etcd-operator-tests-ext
TESTS_EXT_LDFLAGS ?= -X 'main.CommitFromGit=$(shell git rev-parse --short HEAD)' \
                     -X 'main.BuildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)' \
                     -X 'main.GitTreeState=$(shell if git diff-index --quiet HEAD --; then echo clean; else echo dirty; fi)'
GOOS ?= linux
GOARCH ?= amd64

# This will call a macro called "build-image" which will generate image specific targets based on the parameters:
# $0 - macro name
# $1 - target name
# $2 - image ref
# $3 - Dockerfile path
# $4 - context directory for image build
$(call build-image,ocp-cluster-etcd-operator,$(IMAGE_REGISTRY)/ocp/4.4:cluster-etcd-operator, ./Dockerfile.ocp,.)

$(call verify-golang-versions,Dockerfile.ocp)

# Configure the 'telepresence' target
# See vendor/github.com/openshift/build-machinery-go/scripts/run-telepresence.sh for usage and configuration details
export TP_DEPLOYMENT_YAML ?=./manifests/0000_12_etcd-operator_06_deployment.yaml
export TP_CMD_PATH ?=./cmd/cluster-etcd-operator

# This was copied from https://github.com/openshift/cluster-kube-apiserver-operator
# Exclude e2e tests from unit testing
GO_TEST_PACKAGES :=./pkg/... ./cmd/...

test-e2e: GO_TEST_PACKAGES :=./test/e2e/...
test-e2e: GO_TEST_FLAGS += -v
test-e2e: GO_TEST_FLAGS += -timeout 2h
test-e2e: GO_TEST_FLAGS += -p 1
test-e2e: test-unit
.PHONY: test-e2e

# Build the openshift-tests-extension binary
.PHONY: tests-ext-build
tests-ext-build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) GO_COMPLIANCE_POLICY=exempt_all CGO_ENABLED=0 \
	go build -o $(TESTS_EXT_BINARY) -ldflags "$(TESTS_EXT_LDFLAGS)" $(TESTS_EXT_PACKAGE)

# Update test metadata
.PHONY: tests-ext-update
tests-ext-update:
	./$(TESTS_EXT_BINARY) update

# Clean tests extension artifacts
.PHONY: tests-ext-clean
tests-ext-clean:
	rm -f $(TESTS_EXT_BINARY) $(TESTS_EXT_BINARY).gz
