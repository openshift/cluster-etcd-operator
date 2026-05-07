package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"

	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = g.Describe("[sig-etcd] cluster-etcd-operator", func() {

	g.It("[OTP][Operator] OCP-24280 Etcd basic verification", func() {
		ctx := context.Background()
		cs := framework.NewClientSet("")

		g.By("Checking etcd cluster operator status")
		co, err := cs.ConfigV1Interface.ClusterOperators().Get(ctx, "etcd", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		available := findCondition(co.Status.Conditions, configv1.OperatorAvailable)
		o.Expect(available).NotTo(o.BeNil(), "missing Available condition")
		o.Expect(string(available.Status)).To(o.Equal(string(configv1.ConditionTrue)),
			"etcd clusteroperator Available should be True")

		progressing := findCondition(co.Status.Conditions, configv1.OperatorProgressing)
		o.Expect(progressing).NotTo(o.BeNil(), "missing Progressing condition")
		o.Expect(string(progressing.Status)).To(o.Equal(string(configv1.ConditionFalse)),
			"etcd clusteroperator Progressing should be False")

		degraded := findCondition(co.Status.Conditions, configv1.OperatorDegraded)
		o.Expect(degraded).NotTo(o.BeNil(), "missing Degraded condition")
		o.Expect(string(degraded.Status)).To(o.Equal(string(configv1.ConditionFalse)),
			"etcd clusteroperator Degraded should be False")

		g.By("Checking etcd operator pods are running")
		operatorPods, err := cs.CoreV1Interface.Pods("openshift-etcd-operator").List(ctx, metav1.ListOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		for _, pod := range operatorPods.Items {
			o.Expect(string(pod.Status.Phase)).To(o.Equal("Running"),
				fmt.Sprintf("operator pod %s is not running", pod.Name))
		}

		g.By("Checking etcd pods match master node count")
		masterNodes, err := cs.CoreV1Interface.Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/master=",
		})
		o.Expect(err).NotTo(o.HaveOccurred())

		etcdPods, err := cs.CoreV1Interface.Pods("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: "app=etcd",
		})
		o.Expect(err).NotTo(o.HaveOccurred())

		infra, err := cs.ConfigV1Interface.Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		if strings.Contains(string(infra.Status.ControlPlaneTopology), "Arbiter") {
			o.Expect(len(etcdPods.Items)).To(o.Equal(len(masterNodes.Items)+1),
				"mismatch in etcd pods and master nodes for Arbiter topology")
		} else {
			o.Expect(len(etcdPods.Items)).To(o.Equal(len(masterNodes.Items)),
				"mismatch in etcd pods and master nodes")
		}

		g.By("Checking all etcd pods are running")
		for _, pod := range etcdPods.Items {
			o.Expect(string(pod.Status.Phase)).To(o.Equal("Running"),
				fmt.Sprintf("etcd pod %s is not running", pod.Name))
		}
	})

	g.It("[OTP][Operator] OCP-43330 Ensure etcd version is 3.6", func() {
		ctx := context.Background()
		cs := framework.NewClientSet("")

		etcdPods, err := cs.CoreV1Interface.Pods("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: "app=etcd",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(etcdPods.Items).NotTo(o.BeEmpty(), "no etcd pods found")

		podName := etcdPods.Items[0].Name
		g.By(fmt.Sprintf("Checking etcd server version in pod %s via endpoint status", podName))

		cmd := "unset ETCDCTL_ENDPOINTS; etcdctl --command-timeout=30s --endpoints=https://localhost:2379 endpoint status -w json"
		output, err := exec.Command("oc", "rsh", "-n", "openshift-etcd", "-c", "etcd", podName,
			"sh", "-c", cmd).CombinedOutput()
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to run etcdctl endpoint status")

		var results []struct {
			Status struct {
				Version string `json:"version"`
			} `json:"Status"`
		}
		err = json.Unmarshal([]byte(strings.TrimSpace(string(output))), &results)
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to parse endpoint status JSON: %s", string(output))
		o.Expect(results).NotTo(o.BeEmpty(), "empty endpoint status result")

		serverVersion := results[0].Status.Version
		fmt.Fprintf(g.GinkgoWriter, "etcd server version: %s\n", serverVersion)
		o.Expect(serverVersion).To(o.ContainSubstring("3.6"),
			fmt.Sprintf("etcd server version should be 3.6, got %s", serverVersion))
	})

	g.It("[OTP][Operator] OCP-52418 Corrupt check parameter validation", func() {
		ctx := context.Background()
		cs := framework.NewClientSet("")

		etcdPods, err := cs.CoreV1Interface.Pods("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: "app=etcd",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(etcdPods.Items).NotTo(o.BeEmpty())

		pod := etcdPods.Items[0]
		g.By(fmt.Sprintf("Checking etcd pod %s command args", pod.Name))

		var etcdCommand string
		for _, container := range pod.Spec.Containers {
			if container.Name == "etcd" {
				parts := append([]string{}, container.Command...)
				parts = append(parts, container.Args...)
				etcdCommand = strings.Join(parts, " ")
				break
			}
		}

		o.Expect(etcdCommand).NotTo(o.ContainSubstring("experimental-initial-corrupt-check=true"),
			"etcd should not have experimental-initial-corrupt-check=true")
	})

	g.It("[OTP][Operator] OCP-54129 Etcd alerts in monitoring stack", func() {
		ctx := context.Background()
		cs := framework.NewClientSet("")

		g.By("Checking etcd alert rules across all prometheus rulefile configmaps")
		cmList, err := cs.CoreV1Interface.ConfigMaps("openshift-monitoring").List(ctx, metav1.ListOptions{})
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to list configmaps in openshift-monitoring")

		var allData string
		for _, cm := range cmList.Items {
			if !strings.HasPrefix(cm.Name, "prometheus-k8s-rulefiles-") {
				continue
			}
			for _, v := range cm.Data {
				allData += v
			}
		}
		o.Expect(allData).NotTo(o.BeEmpty(), "no prometheus-k8s-rulefiles-* configmaps found")

		o.Expect(allData).To(o.ContainSubstring("etcdHighFsyncDurations"),
			"should have etcdHighFsyncDurations alert")
		o.Expect(allData).To(o.ContainSubstring("etcdDatabaseQuotaLowSpace"),
			"should have etcdDatabaseQuotaLowSpace alert")
		o.Expect(allData).To(o.ContainSubstring("etcdExcessiveDatabaseGrowth"),
			"should have etcdExcessiveDatabaseGrowth alert")
	})

	g.It("[OTP][Operator] OCP-64148 Verify etcd-bootstrap member is removed", func() {
		ctx := context.Background()
		opClient := framework.NewOperatorClient(g.GinkgoTB())

		g.By("Verifying etcd-bootstrap member is removed")
		etcdCR, err := opClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		found := false
		for _, cond := range etcdCR.Status.Conditions {
			if cond.Reason == "BootstrapAlreadyRemoved" {
				found = true
				o.Expect(string(cond.Status)).To(o.Equal("True"),
					"BootstrapAlreadyRemoved condition should be True")
				o.Expect(cond.Message).To(o.ContainSubstring("etcd-bootstrap member is already removed"),
					"should confirm bootstrap member removal")
				break
			}
		}
		o.Expect(found).To(o.BeTrue(), "BootstrapAlreadyRemoved condition not found")
	})

	g.It("[OTP][Operator] OCP-54999 Verify ETCD health on dual-stack cluster [Serial]", func() {
		ctx := context.Background()
		cs := framework.NewClientSet("")

		g.By("Detecting IP stack type")
		network, err := cs.ConfigV1Interface.Networks().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		svcNetwork := strings.Join(network.Spec.ServiceNetwork, ",")
		isDualStack := strings.Count(svcNetwork, ":") >= 2 && strings.Count(svcNetwork, ".") >= 2
		if !isDualStack {
			g.Skip("Skipping: cluster is not dual-stack")
		}

		g.By("Checking etcd operator status on dual-stack cluster")
		co, err := cs.ConfigV1Interface.ClusterOperators().Get(ctx, "etcd", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		available := findCondition(co.Status.Conditions, configv1.OperatorAvailable)
		o.Expect(available).NotTo(o.BeNil())
		o.Expect(string(available.Status)).To(o.Equal(string(configv1.ConditionTrue)),
			"etcd clusteroperator should be Available on dual-stack")

		etcdPods, err := cs.CoreV1Interface.Pods("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: "app=etcd",
		})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Checking all etcd pods are running on dual-stack")
		for _, pod := range etcdPods.Items {
			o.Expect(string(pod.Status.Phase)).To(o.Equal("Running"),
				fmt.Sprintf("etcd pod %s not running on dual-stack", pod.Name))
		}
	})

	g.It("[OTP][Operator][Serial] OCP-52312 cluster-backup.sh does not conflict with static-pod-certs folder", func() {
		ctx := context.Background()
		cs := framework.NewClientSet("")

		g.By("Selecting a master node")
		masterNodes, err := cs.CoreV1Interface.Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/master=",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(masterNodes.Items).NotTo(o.BeEmpty(), "no master nodes found")
		masterNode := masterNodes.Items[0].Name

		g.DeferCleanup(func() {
			g.By("Removing the certs directory")
			exec.Command("oc", "debug", "-q", "node/"+masterNode, "--",
				"chroot", "/host", "rm", "-rf", "/etc/kubernetes/static-pod-certs").CombinedOutput()

			g.By("Removing the backup directory")
			exec.Command("oc", "debug", "-q", "node/"+masterNode, "--",
				"chroot", "/host", "rm", "-rf", "/home/core/assets/backup").CombinedOutput()
		})

		g.By("Creating the static-pod-certs directory to simulate conflict")
		out, err := exec.Command("oc", "debug", "-q", "node/"+masterNode, "--",
			"chroot", "/host", "mkdir", "-p", "/etc/kubernetes/static-pod-certs").CombinedOutput()
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to create certs dir: %s", string(out))

		g.By("Running cluster-backup.sh")
		backupOut, err := exec.Command("oc", "debug", "node/"+masterNode, "--",
			"chroot", "/host", "/usr/local/bin/cluster-backup.sh", "/home/core/assets/backup").CombinedOutput()
		backupStr := string(backupOut)
		fmt.Fprintf(g.GinkgoWriter, "backup output: %s\n", backupStr)
		o.Expect(err).NotTo(o.HaveOccurred(), "cluster-backup.sh failed: %s", backupStr)
		o.Expect(backupStr).To(o.ContainSubstring("Snapshot saved at"),
			"backup should report snapshot saved")

		g.By("Verifying snapshot file was created")
		re := regexp.MustCompile(`/home/core/assets/backup/snapshot.*\.db`)
		snapshotPath := re.FindString(backupStr)
		o.Expect(snapshotPath).NotTo(o.BeEmpty(), "should find snapshot path in backup output")
		fmt.Fprintf(g.GinkgoWriter, "snapshot file: %s\n", snapshotPath)
	})

	g.It("[OTP][Operator][Serial] OCP-57119 SWEET32 vulnerability check on etcd metrics port 9979", func() {
		ctx := context.Background()
		cs := framework.NewClientSet("")

		g.By("Selecting a master node and getting its IP")
		masterNodes, err := cs.CoreV1Interface.Nodes().List(ctx, metav1.ListOptions{
			LabelSelector: "node-role.kubernetes.io/master=",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(masterNodes.Items).NotTo(o.BeEmpty(), "no master nodes found")
		masterNode := masterNodes.Items[0]
		nodeName := masterNode.Name

		var nodeIP string
		for _, addr := range masterNode.Status.Addresses {
			if addr.Type == "InternalIP" {
				nodeIP = addr.Address
				break
			}
		}
		o.Expect(nodeIP).NotTo(o.BeEmpty(), "could not find InternalIP for node %s", nodeName)
		fmt.Fprintf(g.GinkgoWriter, "master node: %s, IP: %s\n", nodeName, nodeIP)

		g.By("Running testssl.sh against etcd metrics port 9979")
		target := nodeIP + ":9979"
		out, err := exec.Command("oc", "debug", "node/"+nodeName, "--",
			"chroot", "/host", "podman", "run", "--rm", "docker.io/drwetter/testssl.sh:3.2", target).CombinedOutput()
		outputStr := string(out)
		fmt.Fprintf(g.GinkgoWriter, "testssl output length: %d bytes\n", len(outputStr))

		// testssl.sh may exit non-zero even when results are valid, so check output content
		o.Expect(outputStr).NotTo(o.BeEmpty(), "testssl.sh produced no output")

		g.By("Verifying SWEET32 is not vulnerable")
		found := false
		for _, line := range strings.Split(outputStr, "\n") {
			if strings.Contains(line, "SWEET32") {
				fmt.Fprintf(g.GinkgoWriter, "SWEET32 line: %s\n", line)
				if strings.Contains(line, "not vulnerable") {
					found = true
				}
				break
			}
		}
		o.Expect(found).To(o.BeTrue(), "etcd metrics port 9979 is vulnerable to SWEET32")
	})
})

func findCondition(conditions []configv1.ClusterOperatorStatusCondition, condType configv1.ClusterStatusConditionType) *configv1.ClusterOperatorStatusCondition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
