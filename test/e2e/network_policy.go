package e2e

import (
	"context"
	"fmt"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	etcdOperatorNamespace = "openshift-etcd-operator"
	etcdNamespace         = "openshift-etcd"
	agnhostImage          = "registry.k8s.io/e2e-test-images/agnhost:2.45"
)

var _ = g.Describe("[sig-etcd] cluster-etcd-operator", func() {
	g.It("[Operator][NetworkPolicy][Serial] should ensure etcd NetworkPolicies are defined", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Validating NetworkPolicies in openshift-etcd-operator")
		validateEtcdOperatorPolicies(ctx, kubeClient)

		g.By("Validating NetworkPolicies in openshift-etcd")
		validateEtcdNamespacePolicies(ctx, kubeClient)
	})

	g.It("[Operator][NetworkPolicy][Serial] should restore etcd NetworkPolicies after delete or mutation", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Deleting policies in openshift-etcd-operator and waiting for restoration")
		restoreNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-dns")
		restoreNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-apiserver")
		restoreNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-etcd")
		restoreNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-monitoring")
		restoreNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-metrics")

		g.By("Mutating policies in openshift-etcd-operator and waiting for reconciliation")
		mutateAndRestoreNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-dns")
		mutateAndRestoreNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-apiserver")
		mutateAndRestoreNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-etcd")
		mutateAndRestoreNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-monitoring")
		mutateAndRestoreNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-metrics")
	})

	g.It("[Operator][NetworkPolicy][Serial] should enforce etcd-operator egress to apiserver", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		clientLabels := map[string]string{"app": "etcd-operator"}
		host := "kubernetes.default.svc"

		g.By("Verifying allowed egress to apiserver on 6443")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, host, 6443, true)

		g.By("Verifying denied egress to apiserver on 12345")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, host, 12345, false)
	})

	g.It("[Operator][NetworkPolicy][Serial] should enforce etcd-operator egress to DNS", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		clientLabels := map[string]string{"app": "etcd-operator"}
		dnsHost := "kube-dns.kube-system.svc"

		g.By("Verifying allowed egress to DNS on TCP 53")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, dnsHost, 53, true)

		g.By("Verifying allowed egress to DNS on UDP 53")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, dnsHost, 53, true)

		g.By("Verifying denied egress to DNS on unauthorized port 12345")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, dnsHost, 12345, false)
	})

	g.It("[Operator][NetworkPolicy][Serial] should enforce etcd-operator egress to etcd", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		clientLabels := map[string]string{"app": "etcd-operator"}
		etcdHost := fmt.Sprintf("etcd.%s.svc", etcdNamespace)

		g.By("Verifying allowed egress to etcd on 2379")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, etcdHost, 2379, true)

		g.By("Verifying allowed egress to etcd on 9987")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, etcdHost, 9987, true)

		g.By("Verifying allowed egress to etcd on 9979")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, etcdHost, 9979, true)

		g.By("Verifying denied egress to etcd on unauthorized port 12345")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, etcdHost, 12345, false)
	})

	g.It("[Operator][NetworkPolicy][Serial] should enforce etcd-operator egress to monitoring", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		clientLabels := map[string]string{"app": "etcd-operator"}
		monitoringHost := "prometheus-operated.monitoring.svc"

		g.By("Verifying allowed egress to monitoring on 9091")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, monitoringHost, 9091, true)

		g.By("Verifying denied egress to monitoring on unauthorized port 12345")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, monitoringHost, 12345, false)
	})

	g.It("[Operator][NetworkPolicy][Serial] should enforce etcd-operator denied egress to unauthorized destinations", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		clientLabels := map[string]string{"app": "etcd-operator"}

		g.By("Verifying denied egress to external unauthorized host")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, "8.8.8.8", 80, false)

		g.By("Verifying denied egress to unauthorized service")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, "unauthorized-service.default.svc", 8080, false)
	})
})

func validateEtcdOperatorPolicies(ctx context.Context, client kubernetes.Interface) {
	policy := getNetworkPolicy(ctx, client, etcdOperatorNamespace, "allow-to-dns")
	requirePodSelectorLabel(policy, "app", "etcd-operator")
	requireEgressPort(policy, corev1.ProtocolTCP, 53)
	requireEgressPort(policy, corev1.ProtocolUDP, 53)
	requireEgressPort(policy, corev1.ProtocolTCP, 5353)
	requireEgressPort(policy, corev1.ProtocolUDP, 5353)

	policy = getNetworkPolicy(ctx, client, etcdOperatorNamespace, "allow-to-apiserver")
	requirePodSelectorLabel(policy, "app", "etcd-operator")
	requireEgressPort(policy, corev1.ProtocolTCP, 6443)

	policy = getNetworkPolicy(ctx, client, etcdOperatorNamespace, "allow-to-etcd")
	requirePodSelectorLabel(policy, "app", "etcd-operator")
	requireEgressPort(policy, corev1.ProtocolTCP, 2379)
	requireEgressPort(policy, corev1.ProtocolTCP, 9987)
	requireEgressPort(policy, corev1.ProtocolTCP, 9979)

	policy = getNetworkPolicy(ctx, client, etcdOperatorNamespace, "allow-to-monitoring")
	requirePodSelectorLabel(policy, "app", "etcd-operator")
	requireEgressPort(policy, corev1.ProtocolTCP, 9091)

	policy = getNetworkPolicy(ctx, client, etcdOperatorNamespace, "allow-to-metrics")
	requirePodSelectorLabel(policy, "app", "etcd-operator")
	requireIngressPort(policy, corev1.ProtocolTCP, 8443)
	requireIngressFromHostNetwork(policy, 8443)
}

func validateEtcdNamespacePolicies(ctx context.Context, client kubernetes.Interface) {
	policies, err := client.NetworkingV1().NetworkPolicies(etcdNamespace).List(ctx, metav1.ListOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(policies.Items).NotTo(o.BeEmpty())
	logPolicyNames(etcdNamespace, policies.Items)

	o.Expect(hasDefaultDeny(policies.Items)).To(o.BeTrue(), "expected a default deny-all policy in %s", etcdNamespace)
	o.Expect(hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 6443)).To(o.BeTrue(), "expected an egress policy allowing TCP/6443 in %s", etcdNamespace)

	if !hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 2379) && !hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 9980) {
		g.Fail(fmt.Sprintf("expected an egress policy allowing TCP/2379 or TCP/9980 in %s", etcdNamespace))
	}

	if !(hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 53) || hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 5353) ||
		hasEgressPortInNamespace(policies.Items, corev1.ProtocolUDP, 53) || hasEgressPortInNamespace(policies.Items, corev1.ProtocolUDP, 5353)) {
		g.Fail(fmt.Sprintf("expected an egress policy allowing DNS ports in %s", etcdNamespace))
	}
}

func getNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, name string) *networkingv1.NetworkPolicy {
	g.GinkgoHelper()
	policy, err := client.NetworkingV1().NetworkPolicies(namespace).Get(ctx, name, metav1.GetOptions{})
	o.Expect(err).NotTo(o.HaveOccurred(), "failed to get NetworkPolicy %s/%s", namespace, name)
	return policy
}

func requirePodSelectorLabel(policy *networkingv1.NetworkPolicy, key, value string) {
	g.GinkgoHelper()
	actual, ok := policy.Spec.PodSelector.MatchLabels[key]
	if !ok || actual != value {
		g.Fail(fmt.Sprintf("%s/%s: expected podSelector %s=%s, got %v", policy.Namespace, policy.Name, key, value, policy.Spec.PodSelector.MatchLabels))
	}
}

func requireIngressPort(policy *networkingv1.NetworkPolicy, protocol corev1.Protocol, port int32) {
	g.GinkgoHelper()
	if !hasPortInIngress(policy.Spec.Ingress, protocol, port) {
		g.Fail(fmt.Sprintf("%s/%s: expected ingress port %s/%d", policy.Namespace, policy.Name, protocol, port))
	}
}

func requireEgressPort(policy *networkingv1.NetworkPolicy, protocol corev1.Protocol, port int32) {
	g.GinkgoHelper()
	if !hasPortInEgress(policy.Spec.Egress, protocol, port) {
		g.Fail(fmt.Sprintf("%s/%s: expected egress port %s/%d", policy.Namespace, policy.Name, protocol, port))
	}
}

func requireIngressFromHostNetwork(policy *networkingv1.NetworkPolicy, port int32) {
	g.GinkgoHelper()
	for _, rule := range policy.Spec.Ingress {
		if !hasPort(rule.Ports, corev1.ProtocolTCP, port) {
			continue
		}
		for _, peer := range rule.From {
			if peer.NamespaceSelector != nil && peer.NamespaceSelector.MatchLabels != nil {
				if _, ok := peer.NamespaceSelector.MatchLabels["policy-group.network.openshift.io/host-network"]; ok {
					return
				}
			}
		}
	}
	g.Fail(fmt.Sprintf("%s/%s: expected ingress from host-network policy group on port %d", policy.Namespace, policy.Name, port))
}

func hasDefaultDeny(policies []networkingv1.NetworkPolicy) bool {
	for _, policy := range policies {
		if len(policy.Spec.PodSelector.MatchLabels) != 0 || len(policy.Spec.PodSelector.MatchExpressions) != 0 {
			continue
		}
		if hasPolicyTypes(policy.Spec.PolicyTypes, networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress) {
			return true
		}
	}
	return false
}

func hasPolicyTypes(policyTypes []networkingv1.PolicyType, expected ...networkingv1.PolicyType) bool {
	expectedSet := map[networkingv1.PolicyType]struct{}{}
	for _, typ := range expected {
		expectedSet[typ] = struct{}{}
	}
	for _, typ := range policyTypes {
		delete(expectedSet, typ)
	}
	return len(expectedSet) == 0
}

func hasEgressPortInNamespace(policies []networkingv1.NetworkPolicy, protocol corev1.Protocol, port int32) bool {
	for _, policy := range policies {
		if hasPortInEgress(policy.Spec.Egress, protocol, port) {
			return true
		}
	}
	return false
}

func hasPortInIngress(rules []networkingv1.NetworkPolicyIngressRule, protocol corev1.Protocol, port int32) bool {
	for _, rule := range rules {
		if hasPort(rule.Ports, protocol, port) {
			return true
		}
	}
	return false
}

func hasPortInEgress(rules []networkingv1.NetworkPolicyEgressRule, protocol corev1.Protocol, port int32) bool {
	for _, rule := range rules {
		if hasPort(rule.Ports, protocol, port) {
			return true
		}
	}
	return false
}

func hasPort(ports []networkingv1.NetworkPolicyPort, protocol corev1.Protocol, port int32) bool {
	for _, p := range ports {
		if p.Port == nil || p.Port.IntValue() != int(port) {
			continue
		}
		if p.Protocol == nil || *p.Protocol == protocol {
			return true
		}
	}
	return false
}

func logPolicyNames(namespace string, policies []networkingv1.NetworkPolicy) {
	g.GinkgoHelper()
	names := make([]string, 0, len(policies))
	for _, policy := range policies {
		names = append(names, policy.Name)
	}
	g.GinkgoWriter.Printf("networkpolicies in %s: %v\n", namespace, names)
}

func restoreNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, name string) {
	g.GinkgoHelper()
	o.Expect(client.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, name, metav1.DeleteOptions{})).NotTo(o.HaveOccurred())
	err := wait.PollImmediate(5*time.Second, 10*time.Minute, func() (bool, error) {
		_, err := client.NetworkingV1().NetworkPolicies(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return true, nil
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for NetworkPolicy %s/%s to be restored", namespace, name)
}

func mutateAndRestoreNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, name string) {
	g.GinkgoHelper()
	original := getNetworkPolicy(ctx, client, namespace, name)
	patch := []byte(`{"spec":{"podSelector":{"matchLabels":{"np-reconcile":"mutated"}}}}`)
	_, err := client.NetworkingV1().NetworkPolicies(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())

	err = wait.PollImmediate(5*time.Second, 10*time.Minute, func() (bool, error) {
		current := getNetworkPolicy(ctx, client, namespace, name)
		return current.Spec.PodSelector.MatchLabels["np-reconcile"] == "" &&
			current.Spec.PodSelector.MatchLabels["np-reconcile"] == original.Spec.PodSelector.MatchLabels["np-reconcile"], nil
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for NetworkPolicy %s/%s spec to be restored", namespace, name)
}

func expectConnectivity(ctx context.Context, client kubernetes.Interface, namespace string, labels map[string]string, host string, port int32, shouldSucceed bool) {
	g.GinkgoHelper()
	err := wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		ok, err := runConnectivityCheck(ctx, client, namespace, labels, host, port)
		if err != nil {
			return false, err
		}
		return ok == shouldSucceed, nil
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "connectivity to %s:%d should be %t", host, port, shouldSucceed)
}

func runConnectivityCheck(ctx context.Context, client kubernetes.Interface, namespace string, labels map[string]string, host string, port int32) (bool, error) {
	g.GinkgoHelper()
	name := fmt.Sprintf("np-client-%s", rand.String(5))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   boolptr(true),
				RunAsUser:      int64ptr(1001),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{
				{
					Name:  "connect",
					Image: agnhostImage,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: boolptr(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						RunAsNonRoot:             boolptr(true),
						RunAsUser:                int64ptr(1001),
					},
					Command: []string{"/agnhost"},
					Args: []string{
						"connect",
						"--protocol=tcp",
						"--timeout=5s",
						fmt.Sprintf("%s:%d", host, port),
					},
				},
			},
		},
	}

	_, err := client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	defer func() {
		_ = client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	}()

	if err := waitForPodCompletion(ctx, client, namespace, name); err != nil {
		return false, err
	}
	completed, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if len(completed.Status.ContainerStatuses) == 0 {
		return false, fmt.Errorf("no container status recorded for pod %s", name)
	}
	exitCode := completed.Status.ContainerStatuses[0].State.Terminated.ExitCode
	g.GinkgoWriter.Printf("client pod %s/%s exitCode=%d\n", namespace, name, exitCode)
	return exitCode == 0, nil
}

func waitForPodCompletion(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	return wait.PollImmediate(2*time.Second, 2*time.Minute, func() (bool, error) {
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed, nil
	})
}

func boolptr(value bool) *bool {
	return &value
}

func int64ptr(value int64) *int64 {
	return &value
}
