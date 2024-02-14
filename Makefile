all: build
.PHONY: all

# Include the library makefile
include $(addprefix ./vendor/github.com/openshift/build-machinery-go/make/, \
	golang.mk \
	targets/openshift/bindata.mk \
	targets/openshift/images.mk \
	targets/openshift/deps-gomod.mk \
	targets/openshift/operator/telepresence.mk \
)

E2E_TIMEOUT ?= 1h
IMAGE_REGISTRY :=registry.svc.ci.openshift.org
# Setting SHELL to bash allows bash commands to be executed by recipes.
# # # Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# This will call a macro called "build-image" which will generate image specific targets based on the parameters:
# $0 - macro name
# $1 - target name
# $2 - image ref
# $3 - Dockerfile path
# $4 - context directory for image build
$(call build-image,ocp-cluster-etcd-operator,$(IMAGE_REGISTRY)/ocp/4.4:cluster-etcd-operator, ./Dockerfile.rhel7,.)

# This will call a macro called "add-bindata" which will generate bindata specific targets based on the parameters:
# $0 - macro name
# $1 - target suffix
# $2 - input dirs
# $3 - prefix
# $4 - pkg
# $5 - output
# It will generate targets {update,verify}-bindata-$(1) logically grouping them in unsuffixed versions of these targets
# and also hooked into {update,verify}-generated for broader integration.
$(call add-bindata,etcd,./bindata/etcd/...,bindata,etcd_assets,pkg/operator/etcd_assets/bindata.go)

$(call verify-golang-versions,Dockerfile.rhel7)

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
test-e2e:
	go test \
	-timeout $(E2E_TIMEOUT) \
	-count 1 \
	-v \
	-p 1 \
	-tags e2e \
	-run "$(TEST)" \
	./test/e2e
.PHONY: test-e2e
