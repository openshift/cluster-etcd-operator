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
TESTS_EXT_DIR := ./cmd/cluster-etcd-operator-tests-ext
TESTS_EXT_OUTPUT_DIR := ./cmd/cluster-etcd-operator-tests-ext

GO_BUILD_PACKAGES :=./cmd/cluster-etcd-operator ./cmd/tnf-setup-runner ./cmd/cluster-etcd-operator-tests-ext

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
