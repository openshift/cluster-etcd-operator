package e2e_test

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func getTestDir() string {
	// test is running in an OpenShift CI Prow job
	if os.Getenv("OPENSHIFT_CI") == "true" {
		return os.Getenv("ARTIFACT_DIR")
	}
	// not running in a CI job
	return "/tmp"
}

func TestAll(t *testing.T) {
	RegisterFailHandler(Fail)

	suiteConfig, reportConfig := GinkgoConfiguration()

	testDir := getTestDir()
	reportConfig.JSONReport = filepath.Join(testDir, "report.json")
	reportConfig.JUnitReport = filepath.Join(testDir, "junit.xml")
	reportConfig.NoColor = true
	reportConfig.VeryVerbose = true
	RunSpecs(t, "Etcd Operator Suite", suiteConfig, reportConfig)
	//os.Exit(m.Run())
}
