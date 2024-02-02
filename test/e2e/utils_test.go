package e2e

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	. "github.com/onsi/gomega"
)

func createDebugPod(debugNodeName string) {
	debugArgs := strings.Split(fmt.Sprintf("debug node/%s %s %s", debugNodeName, "--to-namespace=default", "--as-root=true"), " ")
	output, err := exec.Command("oc", debugArgs...).CombinedOutput()
	if err != nil {
		fmt.Errorf("Fail to create debug pod on master node %s with err: %s", debugNodeName, string(output))
	}
}

// a successful backup will look like this:
// ./backup-backup-happy-path-2023-08-03_152313
// ./backup-backup-happy-path-2023-08-03_152313/static_kuberesources_2023-08-03_152316__POSSIBLY_DIRTY__.tar.gz
// ./backup-backup-happy-path-2023-08-03_152313/snapshot_2023-08-03_152316__POSSIBLY_DIRTY__.db ]
// we assert that there are always at least two files

func backupFilesFound(name string, files []string) bool {
	var matchesTar, matchesSnap bool
	for _, file := range files {
		matchesTarTag, err := regexp.MatchString(`\./backup-`+name+`-.*.tar.gz`, file)
		Expect(err).NotTo(HaveOccurred())
		if matchesTarTag {
			matchesTar = true
		}
		matchesSnapTag, err := regexp.MatchString(`\./backup-`+name+`-.*/snapshot_.*.db`, file)
		Expect(err).NotTo(HaveOccurred())
		if matchesSnapTag {
			matchesSnap = true
		}
	}
	if matchesTar {
		fmt.Printf("Found matching kube resources.\n")
	} else {
		fmt.Printf("Can't find matching kube resources.\n")
	}
	if matchesSnap {
		fmt.Printf("Found matching snapshot.\n")
	} else {
		fmt.Printf("Can't find matching snapshot.\n")
	}

	return matchesTar && matchesSnap 
}
