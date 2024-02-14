//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

var _ = Describe("Etcd master node", Ordered, func() {
	BeforeAll(func() {
		By("creating Kube clients")
	})

	Context("Etcd backup", func() {
		It("Avoid caching etcdctl on cluster-backup.sh OCP-71253", func() {
			
			By("List master node.")
			clientSet := framework.NewClientSet("")
			masterNodes, err := clientSet.CoreV1Interface.Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: masterNodeLabel})
			Expect(err).NotTo(HaveOccurred())
			if err != nil {
				fmt.Errorf("error while listing master nodes")
			}

			By("Create debug pod on first master node")
			debugNodeName := masterNodes.Items[0].Name
			var debugPodName string
			createDebugPod(debugNodeName)
			defer func() {
				fmt.Println("Remove the debug pod now.")
				debugPodStr := strings.Split(fmt.Sprintf("delete pod %s -n default", debugPodName), " ")
				_, err := exec.Command("oc", debugPodStr...).CombinedOutput()
				Expect(err).NotTo(HaveOccurred())
			}()

			By("Wait for debug pod to be in Running phase.")
			err = wait.PollUntilContextTimeout(context.Background(), time.Second, 30*time.Minute, true, func(ctx context.Context) (done bool, err error) {
				pods, err := clientSet.CoreV1Interface.Pods(debugNamespace).List(ctx, metav1.ListOptions{})
				if err != nil && !apierrors.IsNotFound(err) {
					return false, err
				}

				for _, pod := range pods.Items {
					if strings.Contains(pod.Name, "debug") && pod.Status.Phase == v1.PodRunning {
						debugPodName = pod.Name
						return true, nil
					}
				}
				return false, nil
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verify no backup exist")
			cmdAsStr := fmt.Sprintf("ls -l /host%s", backupPath)
			output, err := exec.Command("oc", getOcArgs(debugPodName, cmdAsStr)...).CombinedOutput()
			if !strings.Contains(string(output), "No such file or directory") {
				fmt.Errorf("The backup file is exist already." )
			}

			By("Run backup")
			cmdAsStr = fmt.Sprintf("chroot /host /bin/bash -euxo pipefail /usr/local/bin/cluster-backup.sh --force %s", backupPath)
			output, err = exec.Command("oc", getOcArgs(debugPodName, cmdAsStr)...).CombinedOutput()
			Expect(err).NotTo(HaveOccurred())

			By("Verify backup created")
			cmdAsStr = fmt.Sprintf("find /host%s", backupPath)
			output, err = exec.Command("oc", getOcArgs(debugPodName, cmdAsStr)...).CombinedOutput()
			Expect(err).NotTo(HaveOccurred())
			files := strings.Split(string(output), "\n")
			if !backupFilesFound("backup-happy-path", trimFilesPrefix(files)) {
				fmt.Errorf("can't find backup file.")
			}

			By("Run backup again, and verify no caching etcdctl")
			cmdAsStr = fmt.Sprintf("chroot /host /bin/bash -euxo pipefail /usr/local/bin/cluster-backup.sh --force %s", backupPath)
			output, err = exec.Command("oc", getOcArgs(debugPodName, cmdAsStr)...).CombinedOutput()
			Expect(err).NotTo(HaveOccurred())
			if !strings.Contains(string(output), "etcdctl is already installed") {
				fmt.Errorf("caching etcdctl still exist.")
			}

			DeferCleanup(func() {
				cmdAsStr = fmt.Sprintf("rm -rf /host%s", backupPath)
				output, err1 := exec.Command("oc", getOcArgs(debugPodName, cmdAsStr)...).CombinedOutput()
				if err1 != nil {
					fmt.Errorf("cleanup failed: %s", string(output))
				}
			})
		})
	})
})
