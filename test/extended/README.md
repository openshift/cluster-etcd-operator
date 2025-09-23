# OpenShift Cluster Etcd Operator Tests Extension
===============================================

This repository contains the tests for the OpenShift Cluster Etcd Operator for OpenShift.
These tests run against OpenShift clusters and are meant to be used in the OpenShift CI/CD pipeline.
They use the framework: https://github.com/openshift-eng/openshift-tests-extension

## How to Run the Tests Locally

| Command | Description |
|---------|-------------|
| `make tests-ext-build` | Builds the test extension binary. |
| `./cluster-etcd-operator-tests-ext list` | Lists all available test cases. |
| `./cluster-etcd-operator-tests-ext run-suite <suite-name>` | Runs a test suite. e.g., `openshift/cluster-etcd-operator/conformance/parallel` |
| `./cluster-etcd-operator-tests-ext run-test <test-name>` | Runs one specific test. |

The tests can be run locally using the `cluster-etcd-operator-tests-ext` binary against an OpenShift cluster.
Use the environment variable `KUBECONFIG` to point to your cluster configuration file such as:

```shell
export KUBECONFIG=path/to/kubeconfig
./cluster-etcd-operator-tests-ext run-test <test-name>
```

### Local Test using OCP

1. Use the `Cluster Bot` to create an OpenShift cluster.

**Example:**
```shell
launch 4.20 gcp,techpreview
```

2. Set the `KUBECONFIG` environment variable to point to your OpenShift cluster configuration file.

**Example:**
```shell
mv ~/Downloads/cluster-bot-2025-08-06-082741.kubeconfig ~/.kube/cluster-bot.kubeconfig
export KUBECONFIG=~/.kube/cluster-bot.kubeconfig
```

3. Run the tests using the `cluster-etcd-operator-tests-ext` binary.

**Example:**
```shell
./cluster-etcd-operator-tests-ext run-suite openshift/cluster-etcd-operator/all
```

### Running with JUnit Output

To generate JUnit XML reports for CI integration:

```shell
./cluster-etcd-operator-tests-ext run-suite openshift/cluster-etcd-operator/conformance/parallel --junit-path $(ARTIFACT_DIR)/junit_$(shell date +%Y%m%d-%H%M%S).xml
```

This generates both OTE framework and Ginkgo JUnit XML reports that can be integrated into CI systems.

## Available Test Suites

| Suite Name | Description |
|------------|-------------|
| `openshift/cluster-etcd-operator/conformance/parallel` | Parallel conformance tests |
| `openshift/cluster-etcd-operator/conformance/serial` | Serial conformance tests |
| `openshift/cluster-etcd-operator/optional/slow` | Optional slow tests |
| `openshift/cluster-etcd-operator/all` | All tests |

## CI/CD Integration

The tests are integrated into the OpenShift CI/CD pipeline through the release configuration.
The CI configuration runs the OTE binary and generates JUnit reports for test result tracking.

**Example CI step:**
```yaml
- as: tests-extension
  commands: |
    echo "Build binary cluster-etcd-operator-tests-ext"
    make tests-ext-build
    echo "Running ./cluster-etcd-operator-tests-ext with sanity test"
    ./cluster-etcd-operator-tests-ext run-suite \
      "openshift/cluster-etcd-operator/conformance/parallel" \
      --junit-path ${ARTIFACT_DIR}/junit_report.xml
  from: src
```

## Makefile Commands

| Target | Description |
|--------|-------------|
| `make build` | Builds the operator binary. |
| `make tests-ext-build` | Builds the test extension binary. |
| `make tests-ext-update` | Updates the metadata JSON file and cleans machine-specific codeLocations. |
| `make tests-ext-clean` | Clean tests extension artifacts. |
| `make tests-ext-help` | Run tests extension help. |
| `make tests-ext-sanity` | Run sanity test. |
| `make tests-ext-list` | List available tests. |
| `make tests-ext-info` | Show extension info. |
| `make verify` | Runs formatting, vet, and linter. |

**Note:** Metadata is stored in: `.openshift-tests-extension/openshift_payload_cluster-etcd-operator.json`

## FAQ

### Why don't we have a Dockerfile for `cluster-etcd-operator-tests-ext`?

We do not provide a Dockerfile for `cluster-etcd-operator-tests-ext` because building and shipping a
standalone image for this test binary would introduce unnecessary complexity.

Technically, it is possible to create a new OpenShift component just for the
tests and add a corresponding test image to the payload. However, doing so requires
onboarding a new component, setting up build pipelines, and maintaining image promotion
and test configuration — all of which adds overhead.

From the OpenShift architecture point of view:

1. Tests for payload components are part of the product. Many users (such as storage vendors, or third-party CNIs)
rely on these tests to validate that their solutions are compatible and conformant with OpenShift.

2. Adding new images to the payload comes with significant overhead and cost.
It is generally preferred to include tests in the same image as the component
being tested whenever possible.

### Why do we need to run `make tests-ext-update`?

Running `make tests-ext-update` ensures that each test gets a unique and stable **TestID** over time.

The TestID is used to identify tests across the OpenShift CI/CD pipeline and reporting tools like Sippy.
It helps track test results, detect regressions, and ensures the correct tests are
executed and reported.

This step is important whenever you add, rename, or delete a test.
More information:
- https://github.com/openshift/enhancements/blob/master/enhancements/testing/openshift-tests-extension.md#test-id
- https://github.com/openshift-eng/ci-test-mapping

### How to get help with OTE?

For help with the OpenShift Tests Extension (OTE), you can reach out on the #wg-openshift-tests-extension Slack channel.

## Test Implementation Details

The OTE implementation uses the registry-based approach from the `openshift-tests-extension` framework:

- **Registry Pattern**: Uses `extension.NewRegistry()` and `extension.NewExtension()` for clean, maintainable code
- **JUnit Integration**: Generates both OTE framework and Ginkgo JUnit XML reports
- **Test Discovery**: Automatically discovers and registers Ginkgo tests from the `test/extended` package
- **CI Ready**: Includes proper JUnit reporter configuration for CI integration

## Architecture

The OTE binary (`cluster-etcd-operator-tests-ext`) provides:

1. **Test Discovery**: Lists all available tests and suites
2. **Test Execution**: Runs individual tests or entire suites
3. **Result Reporting**: Generates JSON and JUnit XML output
4. **CI Integration**: Provides standardized output formats for CI systems

The implementation follows OpenShift best practices and integrates seamlessly with the existing CI/CD pipeline.
