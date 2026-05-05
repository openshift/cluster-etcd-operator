package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"
	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"

	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"
	"github.com/openshift/library-go/test/library"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

func waitForEtcdRollout(timeout time.Duration) {
	cs := framework.NewClientSet("")
	etcdPodsClient := cs.CoreV1Interface.Pods("openshift-etcd")

	err := library.WaitForPodsToStabilizeOnTheSameRevision(g.GinkgoTB(), etcdPodsClient, "app=etcd", 5, 1*time.Minute, 5*time.Second, timeout)
	o.Expect(err).NotTo(o.HaveOccurred(), "etcd pods did not stabilize on the same revision")

	waitForEtcdClusterOperatorHealthy(timeout)
}

func waitForEtcdClusterOperatorHealthy(timeout time.Duration) {
	cs := framework.NewClientSet("")
	ctx := context.Background()
	err := wait.PollImmediate(30*time.Second, timeout, func() (bool, error) {
		co, err := cs.ConfigV1Interface.ClusterOperators().Get(ctx, "etcd", metav1.GetOptions{})
		if err != nil {
			fmt.Fprintf(g.GinkgoWriter, "get clusteroperator err, retrying: %v\n", err)
			return false, nil
		}
		avail := findCondition(co.Status.Conditions, configv1.OperatorAvailable)
		prog := findCondition(co.Status.Conditions, configv1.OperatorProgressing)
		deg := findCondition(co.Status.Conditions, configv1.OperatorDegraded)

		if avail == nil || prog == nil || deg == nil {
			fmt.Fprintf(g.GinkgoWriter, "missing conditions, retrying\n")
			return false, nil
		}
		if avail.Status == configv1.ConditionTrue &&
			prog.Status == configv1.ConditionFalse &&
			deg.Status == configv1.ConditionFalse {
			return true, nil
		}
		fmt.Fprintf(g.GinkgoWriter, "etcd operator: Available=%s Progressing=%s Degraded=%s, retrying\n",
			avail.Status, prog.Status, deg.Status)
		return false, nil
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "etcd clusteroperator did not become healthy")
}

func getEtcdContainerEnvValue(podName, envName string) string {
	ctx := context.Background()
	cs := framework.NewClientSet("")

	pod, err := cs.CoreV1Interface.Pods("openshift-etcd").Get(ctx, podName, metav1.GetOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())

	for _, container := range pod.Spec.Containers {
		if container.Name != "etcd" {
			continue
		}
		for _, env := range container.Env {
			if env.Name == envName {
				return env.Value
			}
		}
	}
	g.Fail(fmt.Sprintf("env var %s not found in etcd container of pod %s", envName, podName))
	return ""
}

// runEtcdctlInPod runs an etcdctl subcommand inside the etcd container of the
// given pod, using the local endpoint. Returns the combined stdout/stderr.
func runEtcdctlInPod(podName, subcommand string) string {
	cmd := fmt.Sprintf("unset ETCDCTL_ENDPOINTS; etcdctl --command-timeout=30s --endpoints=https://localhost:2379 %s", subcommand)
	out, err := exec.Command("oc", "rsh", "-n", "openshift-etcd", "-c", "etcd", podName,
		"sh", "-c", cmd).CombinedOutput()
	o.Expect(err).NotTo(o.HaveOccurred(),
		fmt.Sprintf("etcdctl %s failed on pod %s: %s", subcommand, podName, string(out)))
	return string(out)
}

// endpointStatusResult mirrors the JSON output of `etcdctl endpoint status -w json`.
type endpointStatusResult struct {
	Endpoint string `json:"Endpoint"`
	Status   struct {
		DBSize      int64  `json:"dbSize"`
		DBSizeInUse int64  `json:"dbSizeInUse"`
		Leader      uint64 `json:"leader"`
		RaftIndex   uint64 `json:"raftIndex"`
		RaftTerm    uint64 `json:"raftTerm"`
		Header      struct {
			MemberID uint64 `json:"member_id"`
		} `json:"header"`
	} `json:"Status"`
}

// getEtcdEndpointStatus runs `etcdctl endpoint status -w json` on a pod and
// returns the DB size in bytes and whether this member is the leader.
func getEtcdEndpointStatus(podName string) (dbSizeBytes int, isLeader bool) {
	out := runEtcdctlInPod(podName, "endpoint status -w json")

	var results []endpointStatusResult
	err := json.Unmarshal([]byte(strings.TrimSpace(out)), &results)
	if err != nil {
		fmt.Fprintf(g.GinkgoWriter, "WARNING: failed to parse endpoint status JSON for %s: %v\nraw output: %s\n", podName, err, out)
		return 0, false
	}
	if len(results) == 0 {
		fmt.Fprintf(g.GinkgoWriter, "WARNING: empty endpoint status result for %s\n", podName)
		return 0, false
	}

	r := results[0]
	dbSizeBytes = int(r.Status.DBSize)
	isLeader = r.Status.Header.MemberID == r.Status.Leader
	fmt.Fprintf(g.GinkgoWriter, "endpoint status for %s: dbSize=%d memberID=%d leader=%d isLeader=%t\n",
		podName, dbSizeBytes, r.Status.Header.MemberID, r.Status.Leader, isLeader)
	return
}

var _ = g.Describe("[sig-etcd] cluster-etcd-operator", func() {

	g.It("[OTP][Operator][Serial][Disruptive] OCP-66829 Tuning etcd heartbeat and election timeout [Timeout:60m]", func() {
		ctx := context.Background()
		opClient := framework.NewOperatorClient(g.GinkgoTB())
		cs := framework.NewClientSet("")

		g.By("Patching etcd cluster to Standard")
		etcdCR, err := opClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		originalHardwareSpeed := etcdCR.Spec.HardwareSpeed
		originalForceRedeploymentReason := etcdCR.Spec.ForceRedeploymentReason

		etcdCR.Spec.HardwareSpeed = operatorv1.StandardHardwareSpeed
		_, err = opClient.OperatorV1().Etcds().Update(ctx, etcdCR, metav1.UpdateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.DeferCleanup(func() {
			g.By("Restoring etcd controlPlaneHardwareSpeed to original values")
			cr, err := opClient.OperatorV1().Etcds().Get(context.Background(), "cluster", metav1.GetOptions{})
			if err == nil {
				cr.Spec.HardwareSpeed = originalHardwareSpeed
				cr.Spec.ForceRedeploymentReason = originalForceRedeploymentReason
				_, _ = opClient.OperatorV1().Etcds().Update(context.Background(), cr, metav1.UpdateOptions{})
			}
			waitForEtcdRollout(30 * time.Minute)
		})

		g.By("Forcing etcd rollout for Standard")
		etcdCR, err = opClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		etcdCR.Spec.ForceRedeploymentReason = fmt.Sprintf("hardwareSpeedChange-%s", time.Now().Format("2006-01-02T15:04:05"))
		_, err = opClient.OperatorV1().Etcds().Update(ctx, etcdCR, metav1.UpdateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		waitForEtcdRollout(30 * time.Minute)

		g.By("Verifying Standard values (election=1000, heartbeat=100)")
		etcdPods, err := cs.CoreV1Interface.Pods("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: "app=etcd",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(etcdPods.Items).NotTo(o.BeEmpty())

		o.Expect(getEtcdContainerEnvValue(etcdPods.Items[0].Name, "ETCD_ELECTION_TIMEOUT")).To(o.Equal("1000"))
		o.Expect(getEtcdContainerEnvValue(etcdPods.Items[0].Name, "ETCD_HEARTBEAT_INTERVAL")).To(o.Equal("100"))

		g.By("Patching etcd cluster to Slower")
		etcdCR, err = opClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		etcdCR.Spec.HardwareSpeed = operatorv1.SlowerHardwareSpeed
		_, err = opClient.OperatorV1().Etcds().Update(ctx, etcdCR, metav1.UpdateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Forcing etcd rollout for Slower")
		etcdCR, err = opClient.OperatorV1().Etcds().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		etcdCR.Spec.ForceRedeploymentReason = fmt.Sprintf("hardwareSpeedChange-%s", time.Now().Format("2006-01-02T15:04:05"))
		_, err = opClient.OperatorV1().Etcds().Update(ctx, etcdCR, metav1.UpdateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		waitForEtcdRollout(30 * time.Minute)

		g.By("Verifying Slower values (election=2500, heartbeat=500)")
		etcdPods, err = cs.CoreV1Interface.Pods("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: "app=etcd",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(etcdPods.Items).NotTo(o.BeEmpty())

		o.Expect(getEtcdContainerEnvValue(etcdPods.Items[0].Name, "ETCD_ELECTION_TIMEOUT")).To(o.Equal("2500"))
		o.Expect(getEtcdContainerEnvValue(etcdPods.Items[0].Name, "ETCD_HEARTBEAT_INTERVAL")).To(o.Equal("500"))
	})

	g.It("[OTP][Operator][Serial][Disruptive] OCP-71790 Etcd manual defragmentation", func() {
		ctx := context.Background()
		cs := framework.NewClientSet("")

		etcdPods, err := cs.CoreV1Interface.Pods("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: "app=etcd",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(etcdPods.Items).NotTo(o.BeEmpty())

		dbSizeBefore := make(map[string]int)
		dbSizeAfter := make(map[string]int)
		leaderPod := ""

		g.By("Recording DB sizes and identifying leader")
		for _, pod := range etcdPods.Items {
			dbSize, isLeader := getEtcdEndpointStatus(pod.Name)
			dbSizeBefore[pod.Name] = dbSize
			fmt.Fprintf(g.GinkgoWriter, "pod %s: dbSize=%d isLeader=%t\n", pod.Name, dbSize, isLeader)
			if isLeader {
				leaderPod = pod.Name
			}
		}
		o.Expect(leaderPod).NotTo(o.BeEmpty(), "should find an etcd leader")
		fmt.Fprintf(g.GinkgoWriter, "etcd leader: %s\n", leaderPod)

		g.By("Defragmenting non-leader members first")
		for _, pod := range etcdPods.Items {
			if pod.Name == leaderPod {
				continue
			}
			runEtcdctlInPod(pod.Name, "defrag")
			dbSize, _ := getEtcdEndpointStatus(pod.Name)
			dbSizeAfter[pod.Name] = dbSize
		}

		g.By("Defragmenting leader")
		runEtcdctlInPod(leaderPod, "defrag")
		dbSize, _ := getEtcdEndpointStatus(leaderPod)
		dbSizeAfter[leaderPod] = dbSize

		g.By("Verifying DB sizes decreased after defrag")
		for pod, sizeBefore := range dbSizeBefore {
			fmt.Fprintf(g.GinkgoWriter, "pod %s: before=%d after=%d\n", pod, sizeBefore, dbSizeAfter[pod])
			o.Expect(dbSizeAfter[pod]).To(o.BeNumerically("<=", sizeBefore),
				fmt.Sprintf("DB size for %s did not decrease after defrag", pod))
		}

		g.By("Clearing any NOSPACE alarms")
		alarmOut := runEtcdctlInPod(leaderPod, "alarm list")
		if strings.TrimSpace(alarmOut) != "" {
			runEtcdctlInPod(leaderPod, "alarm disarm")
		}
	})

	g.It("[OTP][Operator][Serial][Disruptive] OCP-75224 Manual rotation of etcd signer certs [Timeout:60m]", func() {
		ctx := context.Background()
		cs := framework.NewClientSet("")

		g.By("Recording initial signer certificate expiry")
		secret, err := cs.CoreV1Interface.Secrets("openshift-etcd").Get(ctx, "etcd-signer", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		notAfter0 := secret.Annotations["auth.openshift.io/certificate-not-after"]
		o.Expect(notAfter0).NotTo(o.BeEmpty(), "etcd-signer missing certificate-not-after annotation")
		fmt.Fprintf(g.GinkgoWriter, "initial certificate-not-after: %s\n", notAfter0)

		g.By("Checking cluster health before test")
		err = exec.Command("oc", "adm", "wait-for-stable-cluster",
			"--minimum-stable-period=30s", "--timeout=20m").Run()
		if err != nil {
			g.Skip(fmt.Sprintf("Cluster not healthy before test: %v", err))
		}

		g.DeferCleanup(func() {
			g.By("Verifying cluster health after test")
			err := exec.Command("oc", "adm", "wait-for-stable-cluster",
				"--minimum-stable-period=30s", "--timeout=20m").Run()
			o.Expect(err).NotTo(o.HaveOccurred(), "cluster not healthy after signer rotation")
		})

		g.By("Deleting etcd-signer secret")
		err = cs.CoreV1Interface.Secrets("openshift-etcd").Delete(ctx, "etcd-signer", metav1.DeleteOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Waiting for cluster to stabilize after signer rotation")
		err = exec.Command("oc", "adm", "wait-for-stable-cluster",
			"--minimum-stable-period=30s", "--timeout=40m").Run()
		o.Expect(err).NotTo(o.HaveOccurred(), "cluster did not stabilize after signer deletion")

		g.By("Verifying new signer certificate has later expiry")
		newSecret, err := cs.CoreV1Interface.Secrets("openshift-etcd").Get(ctx, "etcd-signer", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		notAfter1 := newSecret.Annotations["auth.openshift.io/certificate-not-after"]
		o.Expect(notAfter1).NotTo(o.BeEmpty())

		layout := "2006-01-02T15:04:05Z"
		time0, err := time.Parse(layout, notAfter0)
		o.Expect(err).NotTo(o.HaveOccurred())
		time1, err := time.Parse(layout, notAfter1)
		o.Expect(err).NotTo(o.HaveOccurred())

		o.Expect(time1.After(time0)).To(o.BeTrue(),
			fmt.Sprintf("new signer expiry %s should be after old %s", time1, time0))
	})

	g.It("[OTP][Operator][Serial][Disruptive] OCP-75259 Auto rotation of etcd signer certs [Timeout:60m]", func() {
		ctx := context.Background()
		cs := framework.NewClientSet("")

		g.By("Recording initial signer certificate dates")
		secret, err := cs.CoreV1Interface.Secrets("openshift-etcd").Get(ctx, "etcd-signer", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		notBefore0 := secret.Annotations["auth.openshift.io/certificate-not-before"]
		notAfter0 := secret.Annotations["auth.openshift.io/certificate-not-after"]
		o.Expect(notAfter0).NotTo(o.BeEmpty())
		fmt.Fprintf(g.GinkgoWriter, "initial certificate-not-after: %s\n", notAfter0)

		g.By("Checking cluster health before test")
		err = exec.Command("oc", "adm", "wait-for-stable-cluster",
			"--minimum-stable-period=30s", "--timeout=20m").Run()
		if err != nil {
			g.Skip(fmt.Sprintf("Cluster not healthy before test: %v", err))
		}

		g.DeferCleanup(func() {
			g.By("Verifying cluster health after test")
			err := exec.Command("oc", "adm", "wait-for-stable-cluster",
				"--minimum-stable-period=30s", "--timeout=20m").Run()
			o.Expect(err).NotTo(o.HaveOccurred(), "cluster not healthy after auto rotation")
		})

		g.By("Setting certificate-not-after to notBefore to trigger rotation")
		secret.Annotations["auth.openshift.io/certificate-not-after"] = notBefore0
		_, err = cs.CoreV1Interface.Secrets("openshift-etcd").Update(ctx, secret, metav1.UpdateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Waiting for cluster to stabilize after auto rotation")
		err = exec.Command("oc", "adm", "wait-for-stable-cluster",
			"--minimum-stable-period=30s", "--timeout=30m").Run()
		o.Expect(err).NotTo(o.HaveOccurred(), "cluster did not stabilize after auto rotation")

		g.By("Verifying new signer certificate has later expiry")
		newSecret, err := cs.CoreV1Interface.Secrets("openshift-etcd").Get(ctx, "etcd-signer", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		notAfter1 := newSecret.Annotations["auth.openshift.io/certificate-not-after"]
		o.Expect(notAfter1).NotTo(o.BeEmpty())

		layout := "2006-01-02T15:04:05Z"
		time0, err := time.Parse(layout, notAfter0)
		o.Expect(err).NotTo(o.HaveOccurred())
		time1, err := time.Parse(layout, notAfter1)
		o.Expect(err).NotTo(o.HaveOccurred())

		o.Expect(time1.After(time0)).To(o.BeTrue(),
			fmt.Sprintf("new signer expiry %s should be after old %s", time1, time0))
	})
})
