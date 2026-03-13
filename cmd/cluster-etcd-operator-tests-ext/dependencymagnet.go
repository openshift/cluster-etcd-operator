// This file ensures test packages and build dependencies are compiled and available.
package main

import (
	// Import test packages to register Ginkgo tests
	_ "github.com/openshift/cluster-etcd-operator/test/e2e"
	// Import build-machinery-go to ensure it's available for Makefile
	_ "github.com/openshift/build-machinery-go"
)
