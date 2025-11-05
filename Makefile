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

# -------------------------------------------------------------------
# OpenShift Tests Extension (Cluster Etcd Operator)
# -------------------------------------------------------------------
TESTS_EXT_BINARY := cluster-etcd-operator-tests-ext
TESTS_EXT_DIR := ./test/extended/tests-extension

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

# -------------------------------------------------------------------
# Build the test extension binary
# -------------------------------------------------------------------
.PHONY: tests-ext-build
tests-ext-build:
	$(MAKE) -C $(TESTS_EXT_DIR) build

# -------------------------------------------------------------------
# Run "update" and strip env-specific metadata
# -------------------------------------------------------------------
.PHONY: tests-ext-update
tests-ext-update:
	$(MAKE) -C $(TESTS_EXT_DIR) build-update

# -------------------------------------------------------------------
# Clean test extension binaries
# -------------------------------------------------------------------
.PHONY: tests-ext-clean
tests-ext-clean:
	$(MAKE) -C $(TESTS_EXT_DIR) clean

# -------------------------------------------------------------------
# Run test suite
# -------------------------------------------------------------------
.PHONY: run-suite
run-suite:
	$(MAKE) -C $(TESTS_EXT_DIR) run-suite SUITE=$(SUITE) JUNIT_DIR=$(JUNIT_DIR)
