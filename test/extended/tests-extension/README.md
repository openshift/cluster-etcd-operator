# Cluster Etcd Operator Tests Extension
========================

This repository contains the tests for the Cluster Etcd Operator for OpenShift.
These tests run against OpenShift clusters and are meant to be used in the OpenShift CI/CD pipeline.
They use the framework: https://github.com/openshift-eng/openshift-tests-extension

## Quick Start

### Building the Test Extension

From the repository root:
```bash
make tests-ext-build
```

Or from the test extension directory:
```bash
cd test/extended/tests-extension
make build
```

The binary will be located at: `test/extended/tests-extension/bin/cluster-etcd-operator-tests-ext`

### Running Tests

| Command                                                                    | Description                                                              |
|----------------------------------------------------------------------------|--------------------------------------------------------------------------|
| `make tests-ext-build`                                                     | Builds the test extension binary (from root).                           |
| `make run-suite SUITE=<suite-name> [JUNIT_DIR=<dir>]`                     | Runs a test suite from root (e.g., `SUITE=openshift/cluster-etcd-operator/conformance/parallel`). |
| `./test/extended/tests-extension/bin/cluster-etcd-operator-tests-ext list`  | Lists all available test cases.                                          |
| `./test/extended/tests-extension/bin/cluster-etcd-operator-tests-ext run-suite <suite-name>` | Runs a test suite directly. |
| `./test/extended/tests-extension/bin/cluster-etcd-operator-tests-ext run-test <test-name>` | Runs one specific test. |

## How to Run the Tests Locally

The tests can be run locally using the `cluster-etcd-operator-tests-ext` binary against an OpenShift cluster.
Use the environment variable `KUBECONFIG` to point to your cluster configuration file such as:

```shell
export KUBECONFIG=path/to/kubeconfig
./test/extended/tests-extension/bin/cluster-etcd-operator-tests-ext run-test <test-name>
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
./test/extended/tests-extension/bin/cluster-etcd-operator-tests-ext run-suite openshift/cluster-etcd-operator/all
```

Or using make from the root directory:
```shell
make run-suite SUITE=openshift/cluster-etcd-operator/all JUNIT_DIR=/tmp/junit-results
```

## Test Module Structure

The test extension has been isolated into its own Go module to separate test dependencies from production code:

```
test/extended/tests-extension/
├── bin/                           # Test binaries (gitignored)
├── cmd/                           # Test extension main package
├── .openshift-tests-extension/    # Test metadata
├── go.mod                         # Separate module with test dependencies
├── go.sum
├── Makefile                       # Test-specific build targets
├── main.go                        # Ginkgo test specs
└── README.md
```

### Key Benefits of Dependency Isolation

- **Smaller production images**: Test dependencies (ginkgo, gomega, etc.) are not included in production builds
- **Faster builds**: Production builds don't need to vendor test dependencies
- **Cleaner dependency management**: Test dependencies isolated in `test/extended/tests-extension/go.mod`
- **Better CI performance**: Smaller images, faster pulls, less storage

## Writing Tests

You can write tests in the `test/extended/tests-extension/` directory.

## Development Workflow

- Add or update tests in: `test/extended/tests-extension/`
- Run `make build` to build the operator binary and `make tests-ext-build` for the test binary.
- You can run the full suite or one test using the commands in the table above.
- Before committing your changes:
    - Run `make tests-ext-update` (updates test metadata)
    - Run `make verify` to check formatting, linting, and validation

## How to Rename a Test

1. Run `./test/extended/tests-extension/bin/cluster-etcd-operator-tests-ext list` to see the current test names
2. Find the name of the test you want to rename
3. Add a Ginkgo label with the original name, like this:

```go
It("should pass a renamed sanity check",
	Label("original-name:[sig-etcd] My Old Test Name"),
	func(ctx context.Context) {
		Expect(len("test")).To(BeNumerically(">", 0))
	})
```

4. Run `make tests-ext-update` to update the metadata

**Note:** Only add the label once. Do not update it again after future renames.

## How to Delete a Test

1. Run `./test/extended/tests-extension/bin/cluster-etcd-operator-tests-ext list` to find the test name
2. Add the test name to the `IgnoreObsoleteTests` block in `test/extended/tests-extension/cmd/main.go`, like this:

```go
ext.IgnoreObsoleteTests(
    "[sig-etcd] My removed test name",
)
```

3. Delete the test code from your suite.
4. Run `make tests-ext-update` to clean the metadata

**WARNING**: Deleting a test may cause issues with Sippy https://sippy.dptools.openshift.org/sippy-ng/
or other tools that expected the Unique TestID tracked outside of this repository. [More info](https://github.com/openshift-eng/ci-test-mapping)
Check the status of https://issues.redhat.com/browse/TRT-2208 before proceeding with test deletions.

## E2E Test Configuration

Tests are configured in the `openshift/release` repository, under `ci-operator/config/openshift/cluster-etcd-operator`.

## Makefile Commands

### Root Makefile (from repository root)

| Target                   | Description                                                                  |
|--------------------------|------------------------------------------------------------------------------|
| `make build`             | Builds the operator binary.                                                      |
| `make tests-ext-build`   | Builds the test extension binary (delegates to test Makefile).              |
| `make tests-ext-update`  | Updates test metadata (delegates to test Makefile).                         |
| `make tests-ext-clean`   | Cleans test extension binaries (delegates to test Makefile).                |
| `make run-suite SUITE=<name> [JUNIT_DIR=<dir>]` | Runs a test suite (delegates to test Makefile). |
| `make verify`            | Runs formatting, vet, and linter.                                            |

### Test Extension Makefile (from test/extended/tests-extension/)

| Target                   | Description                                                                  |
|--------------------------|------------------------------------------------------------------------------|
| `make build`             | Builds the test extension binary to `bin/cluster-etcd-operator-tests-ext`. |
| `make update-metadata`   | Builds and updates test metadata JSON file.                                  |
| `make build-update`      | Builds binary and updates metadata (cleans machine-specific codeLocations).  |
| `make clean`             | Removes the `bin/` directory.                                                |
| `make run-suite SUITE=<name> [JUNIT_DIR=<dir>]` | Runs a specific test suite with optional JUnit XML output. |
| `make list-test-names`   | Lists all test names.                                                        |
| `make verify-metadata`   | Verifies metadata is up to date.                                             |

**Note:** Metadata is stored in: `test/extended/tests-extension/.openshift-tests-extension/openshift_payload_cluster-etcd-operator.json`

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
