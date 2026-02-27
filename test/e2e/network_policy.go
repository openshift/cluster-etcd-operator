package e2e

import (
	"context"
	"fmt"
	"net"
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

// formatHostPort formats an IP address and port for network connectivity.
// IPv6 addresses are wrapped in brackets: [::1]:443
// IPv4 addresses remain as-is: 192.168.1.1:443
func formatHostPort(ip string, port int32) string {
	if net.ParseIP(ip).To4() == nil && net.ParseIP(ip) != nil {
		// IPv6 address - needs brackets
		return fmt.Sprintf("[%s]:%d", ip, port)
	}
	// IPv4 address or hostname
	return fmt.Sprintf("%s:%d", ip, port)
}

// isIPv6Cluster checks if the cluster is using IPv6 by examining the kubernetes service ClusterIP
func isIPv6Cluster(ctx context.Context, client kubernetes.Interface) bool {
	kubeSvc, err := client.CoreV1().Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return false
	}
	ip := net.ParseIP(kubeSvc.Spec.ClusterIP)
	return ip != nil && ip.To4() == nil
}

// getExternalTestIP returns an appropriate external IP for connectivity testing
// IPv4: 8.8.8.8 (Google DNS)
// IPv6: 2001:4860:4860::8888 (Google DNS IPv6)
func getExternalTestIP(ctx context.Context, client kubernetes.Interface) string {
	if isIPv6Cluster(ctx, client) {
		return "2001:4860:4860::8888"
	}
	return "8.8.8.8"
}

var _ = g.Describe("[sig-etcd] cluster-etcd-operator", func() {
	g.It("[Operator][NetworkPolicy] should ensure etcd NetworkPolicies are defined", func() {
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

	g.It("[Operator][NetworkPolicy][Serial] should restore etcd NetworkPolicies after delete or mutation[Timeout:30m]", func() {
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

	g.It("[Operator][NetworkPolicy] should enforce etcd-operator egress to apiserver", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		// Get kubernetes service ClusterIP to avoid DNS issues
		kubeSvc, err := kubeClient.CoreV1().Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		apiServerIP := kubeSvc.Spec.ClusterIP
		g.GinkgoWriter.Printf("Kubernetes API Server ClusterIP: %s\n", apiServerIP)

		clientLabels := map[string]string{"app": "test-connectivity"}

		g.GinkgoWriter.Printf("\n=== Testing API Server Egress ===\n")
		g.GinkgoWriter.Printf("Note: allow-to-apiserver policy has unrestricted egress [{}], allows all destinations/ports\n")
		g.By("Verifying allowed egress to apiserver on 443 (kubernetes service port)")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, apiServerIP, 443, true)
		g.GinkgoWriter.Printf("=== API Server Egress Test Complete ===\n")
	})

	g.It("[Operator][NetworkPolicy] should enforce etcd-operator egress to DNS", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		// Get DNS service ClusterIP to avoid DNS chicken-and-egg problem
		dnsSvc, err := kubeClient.CoreV1().Services("openshift-dns").Get(ctx, "dns-default", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		dnsIP := dnsSvc.Spec.ClusterIP
		g.GinkgoWriter.Printf("DNS Service ClusterIP: %s\n", dnsIP)
		g.GinkgoWriter.Printf("DNS Service Ports: ")
		for _, port := range dnsSvc.Spec.Ports {
			g.GinkgoWriter.Printf("%s/%d ", port.Protocol, port.Port)
		}
		g.GinkgoWriter.Printf("\n")

		clientLabels := map[string]string{"app": "test-connectivity"}

		g.GinkgoWriter.Printf("\n=== Testing DNS Egress ===\n")
		g.By("Verifying allowed egress to DNS on TCP 53")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, dnsIP, 53, true)

		g.GinkgoWriter.Printf("\nNote: Port 5353 is allowed by network policy but not exposed by dns-default service\n")
		g.GinkgoWriter.Printf("The allow-to-dns policy permits ports 53, 5353 (TCP/UDP) for direct pod-to-pod communication\n")
		g.GinkgoWriter.Printf("=== DNS Egress Test Complete ===\n")
	})

	g.It("[Operator][NetworkPolicy] should enforce etcd-operator egress to etcd", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		// Get etcd service ClusterIP
		etcdSvc, err := kubeClient.CoreV1().Services(etcdNamespace).Get(ctx, "etcd", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		etcdIP := etcdSvc.Spec.ClusterIP
		g.GinkgoWriter.Printf("etcd Service ClusterIP: %s\n", etcdIP)
		g.GinkgoWriter.Printf("etcd Service Ports: ")
		for _, port := range etcdSvc.Spec.Ports {
			g.GinkgoWriter.Printf("%s/%d ", port.Name, port.Port)
		}
		g.GinkgoWriter.Printf("\n")

		g.GinkgoWriter.Printf("\n=== Testing etcd Egress ===\n")
		g.GinkgoWriter.Printf("Note: etcd requires mutual TLS authentication\n")
		g.GinkgoWriter.Printf("Simple TCP connectivity tests timeout without proper TLS handshake\n")
		g.GinkgoWriter.Printf("Policy validation ensures network policy correctly allows ports 2379, 9978, 9979, 9980\n")

		// Validate the policy structure instead of connectivity
		policy := getNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-etcd")
		g.By("Validating allow-to-etcd policy configuration")
		requirePodSelectorLabel(policy, "app", "etcd-operator")
		g.GinkgoWriter.Printf("  - Policy targets app=etcd-operator pods\n")

		requireEgressPort(policy, corev1.ProtocolTCP, 2379)
		g.GinkgoWriter.Printf("  - Allows egress on TCP/2379 (etcd client)\n")

		requireEgressPort(policy, corev1.ProtocolTCP, 9978)
		g.GinkgoWriter.Printf("  - Allows egress on TCP/9978 (metrics server)\n")

		requireEgressPort(policy, corev1.ProtocolTCP, 9979)
		g.GinkgoWriter.Printf("  - Allows egress on TCP/9979 (metrics proxy)\n")

		requireEgressPort(policy, corev1.ProtocolTCP, 9980)
		g.GinkgoWriter.Printf("  - Allows egress on TCP/9980 (health check)\n")

		g.GinkgoWriter.Printf("=== etcd Egress Policy Validated ===\n")
	})

	g.It("[Operator][NetworkPolicy] should enforce etcd-operator egress to monitoring", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		// Try to get thanos-querier service (commonly used for monitoring queries)
		monitoringSvc, err := kubeClient.CoreV1().Services("openshift-monitoring").Get(ctx, "thanos-querier", metav1.GetOptions{})
		if err != nil {
			g.GinkgoWriter.Printf("Skipping monitoring test: thanos-querier service not found: %v\n", err)
			g.Skip("thanos-querier service not available")
		}
		monitoringIP := monitoringSvc.Spec.ClusterIP
		g.GinkgoWriter.Printf("Monitoring Service ClusterIP: %s\n", monitoringIP)

		clientLabels := map[string]string{"app": "etcd-operator"}

		g.GinkgoWriter.Printf("\n=== Testing Monitoring Egress ===\n")
		g.By("Verifying allowed egress to monitoring on 9091 (Thanos)")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, monitoringIP, 9091, true)
		g.GinkgoWriter.Printf("=== Monitoring Egress Test Complete ===\n")
	})

	g.It("[Operator][NetworkPolicy] should allow unrestricted egress due to allow-to-apiserver policy", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		clientLabels := map[string]string{"app": "test-connectivity"}
		externalIP := getExternalTestIP(ctx, kubeClient)
		ipFamily := "IPv4"
		if isIPv6Cluster(ctx, kubeClient) {
			ipFamily = "IPv6"
		}

		g.GinkgoWriter.Printf("\n=== Testing Unrestricted Egress ===\n")
		g.GinkgoWriter.Printf("Cluster IP family: %s\n", ipFamily)
		g.GinkgoWriter.Printf("Note: allow-to-apiserver policy has podSelector {} and egress [{}]\n")
		g.GinkgoWriter.Printf("This allows ALL pods to egress to ANY destination on ANY port\n")

		g.By(fmt.Sprintf("Verifying allowed egress to external host (%s:53) due to unrestricted policy", externalIP))
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, externalIP, 53, true)

		g.GinkgoWriter.Printf("=== Unrestricted Egress Test Complete ===\n")
	})

	g.It("[Operator][NetworkPolicy] should allow ingress to etcd-operator metrics endpoint", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.GinkgoWriter.Printf("\n=== Validating allow-to-metrics ingress policy ===\n")
		policy := getNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-metrics")
		g.GinkgoWriter.Printf("  - Policy found: %s/%s\n", policy.Namespace, policy.Name)

		g.By("Validating pod selector targets etcd-operator")
		requirePodSelectorLabel(policy, "app", "etcd-operator")
		g.GinkgoWriter.Printf("  - PodSelector: app=etcd-operator (targets operator pods)\n")

		g.By("Validating policy type is Ingress")
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeIngress), "should have Ingress policy type")
		g.GinkgoWriter.Printf("  - PolicyTypes: %v\n", policy.Spec.PolicyTypes)

		g.By("Validating ingress rules")
		o.Expect(policy.Spec.Ingress).To(o.HaveLen(1), "should have exactly one ingress rule")
		g.GinkgoWriter.Printf("  - Number of ingress rules: %d\n", len(policy.Spec.Ingress))

		requireIngressPort(policy, corev1.ProtocolTCP, 8443)
		g.GinkgoWriter.Printf("  - Ingress port: TCP/8443 (operator metrics)\n")

		// Validate that ingress allows from ANY source (no 'from' field restriction)
		g.By("Validating ingress allows from any source")
		ingressRule := policy.Spec.Ingress[0]
		o.Expect(ingressRule.From).To(o.BeEmpty(), "metrics policy should allow ingress from any source")
		g.GinkgoWriter.Printf("  - Ingress from: ANY source (no 'from' field restriction)\n")
		g.GinkgoWriter.Printf("  - This allows Prometheus and other monitoring components to scrape metrics\n")

		g.By("Validating no egress rules defined")
		o.Expect(policy.Spec.Egress).To(o.BeEmpty(), "metrics policy should only have ingress rules")
		g.GinkgoWriter.Printf("  - Egress rules: none (ingress-only policy)\n")

		g.GinkgoWriter.Printf("=== Metrics ingress policy structure validated successfully ===\n")

		// Test actual connectivity
		g.GinkgoWriter.Printf("\n=== Testing ingress connectivity to metrics endpoint ===\n")

		// Get etcd-operator pod IP
		pods, err := kubeClient.CoreV1().Pods(etcdOperatorNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=etcd-operator",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(pods.Items).NotTo(o.BeEmpty(), "etcd-operator pod should exist")
		operatorPodIP := pods.Items[0].Status.PodIP
		g.GinkgoWriter.Printf("  - etcd-operator pod IP: %s\n", operatorPodIP)

		// Test from any namespace - should be allowed since policy has no 'from' restriction
		g.By("Testing allowed ingress from any namespace")
		testNS := "default"
		clientLabels := map[string]string{"test": "ingress-test"}
		g.GinkgoWriter.Printf("  Testing from namespace: %s (policy allows from ANY source)\n", testNS)
		expectConnectivity(ctx, kubeClient, testNS, clientLabels, operatorPodIP, 8443, true)

		g.GinkgoWriter.Printf("=== Metrics ingress policy working correctly ===\n")
	})

	g.It("[Operator][NetworkPolicy] should ensure default-deny policy in openshift-etcd-operator", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.GinkgoWriter.Printf("\n=== Validating default-deny policy in %s ===\n", etcdOperatorNamespace)
		policy := getNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "default-deny")
		g.GinkgoWriter.Printf("  - Policy found: %s/%s\n", policy.Namespace, policy.Name)

		g.By("Validating pod selector is empty (applies to all pods)")
		o.Expect(policy.Spec.PodSelector.MatchLabels).To(o.BeEmpty(), "default-deny should select all pods")
		o.Expect(policy.Spec.PodSelector.MatchExpressions).To(o.BeEmpty(), "default-deny should select all pods")
		g.GinkgoWriter.Printf("  - PodSelector: {} (applies to all pods in namespace)\n")

		g.By("Validating policy types include both Ingress and Egress")
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeIngress), "default-deny should include Ingress")
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeEgress), "default-deny should include Egress")
		g.GinkgoWriter.Printf("  - PolicyTypes: [Ingress, Egress]\n")
		g.GinkgoWriter.Printf("  - With empty podSelector and no allow rules, this denies all ingress and egress by default\n")
		g.GinkgoWriter.Printf("  - Other policies then add specific allow rules (OR logic)\n")
		g.GinkgoWriter.Printf("=== Default-deny policy validated successfully ===\n")
	})

	g.It("[Operand][NetworkPolicy] should ensure default-deny policy in openshift-etcd", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.GinkgoWriter.Printf("\n=== Validating default-deny policy in %s ===\n", etcdNamespace)
		policy := getNetworkPolicy(ctx, kubeClient, etcdNamespace, "default-deny")
		g.GinkgoWriter.Printf("  - Policy found: %s/%s\n", policy.Namespace, policy.Name)

		g.By("Validating pod selector is empty (applies to all pods)")
		o.Expect(policy.Spec.PodSelector.MatchLabels).To(o.BeEmpty(), "default-deny should select all pods")
		o.Expect(policy.Spec.PodSelector.MatchExpressions).To(o.BeEmpty(), "default-deny should select all pods")
		g.GinkgoWriter.Printf("  - PodSelector: {} (applies to all pods in namespace)\n")

		g.By("Validating policy types include both Ingress and Egress")
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeIngress), "default-deny should include Ingress")
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeEgress), "default-deny should include Egress")
		g.GinkgoWriter.Printf("  - PolicyTypes: [Ingress, Egress]\n")
		g.GinkgoWriter.Printf("  - Note: etcd static pods use hostNetwork and bypass NetworkPolicy\n")
		g.GinkgoWriter.Printf("  - This policy affects guard, installer, and pruner pods on pod network\n")
		g.GinkgoWriter.Printf("=== Default-deny policy validated successfully ===\n")
	})

	g.It("[Operand][NetworkPolicy] should allow guard pods to access etcd health endpoints", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.GinkgoWriter.Printf("\n=== Validating allow-guard-to-etcd policy ===\n")
		policy := getNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-guard-to-etcd")
		g.GinkgoWriter.Printf("  - Policy found: %s/%s\n", policy.Namespace, policy.Name)

		g.By("Validating pod selector targets guard pods")
		requirePodSelectorLabel(policy, "app", "guard")
		g.GinkgoWriter.Printf("  - PodSelector: app=guard\n")

		g.By("Validating egress to etcd client port 2379")
		requireEgressPort(policy, corev1.ProtocolTCP, 2379)
		g.GinkgoWriter.Printf("  - Egress port: TCP/2379 (etcd client)\n")

		g.By("Validating egress to health check port 9980")
		requireEgressPort(policy, corev1.ProtocolTCP, 9980)
		g.GinkgoWriter.Printf("  - Egress port: TCP/9980 (health check/readyz)\n")

		g.By("Validating egress allows to any destination (no 'to' field restriction)")
		o.Expect(policy.Spec.Egress).To(o.HaveLen(1), "should have exactly one egress rule")
		egressRule := policy.Spec.Egress[0]
		o.Expect(egressRule.To).To(o.BeEmpty(), "should allow egress to any destination on specified ports")
		g.GinkgoWriter.Printf("  - Egress 'to': ANY destination (no 'to' field restriction)\n")
		g.GinkgoWriter.Printf("  - Note: etcd static pods use hostNetwork and are accessed via node IPs\n")
		g.GinkgoWriter.Printf("  - Guard pods need to reach etcd on varying node IPs, so no destination restriction\n")
		g.GinkgoWriter.Printf("  - Defense in depth: port restrictions (2379, 9980) still apply\n")

		g.GinkgoWriter.Printf("=== Guard policy structure validated successfully ===\n")

		// Test connectivity
		g.GinkgoWriter.Printf("\n=== Testing guard pod egress connectivity ===\n")
		etcdSvc, err := kubeClient.CoreV1().Services(etcdNamespace).Get(ctx, "etcd", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		etcdIP := etcdSvc.Spec.ClusterIP
		g.GinkgoWriter.Printf("  - etcd service IP: %s\n", etcdIP)

		guardLabels := map[string]string{"app": "guard"}

		g.By("Testing guard pod can access etcd health endpoint on 9980")
		expectConnectivity(ctx, kubeClient, etcdNamespace, guardLabels, etcdIP, 9980, true)

		g.GinkgoWriter.Printf("=== Guard pod connectivity validated successfully ===\n")
	})

	g.It("[Operand][NetworkPolicy] should allow installer and pruner pods to access apiserver", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.GinkgoWriter.Printf("\n=== Validating allow-installer-pruner-to-apiserver policy ===\n")
		policy := getNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-installer-pruner-to-apiserver")
		g.GinkgoWriter.Printf("  - Policy found: %s/%s\n", policy.Namespace, policy.Name)

		g.By("Validating egress rules")
		// Log actual egress for debugging
		for i, rule := range policy.Spec.Egress {
			g.GinkgoWriter.Printf("  - Egress rule %d: ports=%d, to=%d\n", i, len(rule.Ports), len(rule.To))
			for _, p := range rule.Ports {
				if p.Port != nil && p.Protocol != nil {
					g.GinkgoWriter.Printf("    Port: %s/%d\n", *p.Protocol, p.Port.IntValue())
				}
			}
			if len(rule.Ports) == 0 && len(rule.To) == 0 {
				g.GinkgoWriter.Printf("    Unrestricted egress\n")
			}
		}
		if hasPortInEgress(policy.Spec.Egress, corev1.ProtocolTCP, 6443) {
			g.GinkgoWriter.Printf("  - Egress: TCP/6443 (API server)\n")
		} else if len(policy.Spec.Egress) > 0 && len(policy.Spec.Egress[0].Ports) == 0 {
			g.GinkgoWriter.Printf("  - Egress: unrestricted (allows all destinations/ports)\n")
		} else {
			g.Fail(fmt.Sprintf("allow-installer-pruner-to-apiserver: expected egress port TCP/6443 or unrestricted egress, got %v", policy.Spec.Egress))
		}

		g.By("Validating pod selector")
		if len(policy.Spec.PodSelector.MatchExpressions) > 0 {
			found := false
			for _, expr := range policy.Spec.PodSelector.MatchExpressions {
				if expr.Key == "app" && expr.Operator == metav1.LabelSelectorOpIn {
					o.Expect(expr.Values).To(o.ContainElement("installer"), "should include installer")
					o.Expect(expr.Values).To(o.ContainElement("pruner"), "should include pruner")
					g.GinkgoWriter.Printf("  - PodSelector: app in (installer, pruner)\n")
					found = true
					break
				}
			}
			o.Expect(found).To(o.BeTrue(), "should have app matchExpression with installer and pruner")
		} else if len(policy.Spec.PodSelector.MatchLabels) == 0 {
			g.GinkgoWriter.Printf("  - PodSelector: {} (applies to all pods)\n")
		} else {
			g.GinkgoWriter.Printf("  - PodSelector matchLabels: %v\n", policy.Spec.PodSelector.MatchLabels)
		}
		g.GinkgoWriter.Printf("=== Installer/Pruner policy validated successfully ===\n")
	})

	g.It("[Operand][NetworkPolicy] should allow operand helper pods to access DNS", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.GinkgoWriter.Printf("\n=== Validating allow-operand-helpers-to-dns policy ===\n")
		policy := getNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-operand-helpers-to-dns")
		g.GinkgoWriter.Printf("  - Policy found: %s/%s\n", policy.Namespace, policy.Name)

		g.By("Validating egress to DNS ports 53 and 5353")
		requireEgressPort(policy, corev1.ProtocolTCP, 53)
		requireEgressPort(policy, corev1.ProtocolUDP, 53)
		requireEgressPort(policy, corev1.ProtocolTCP, 5353)
		requireEgressPort(policy, corev1.ProtocolUDP, 5353)
		g.GinkgoWriter.Printf("  - Egress ports: TCP/53, UDP/53, TCP/5353, UDP/5353\n")
		g.GinkgoWriter.Printf("  - Standard DNS (53) and mDNS/CoreDNS (5353) supported\n")

		g.By("Validating pod selector targets guard, installer, and pruner pods")
		// Verify podSelector uses matchExpressions for guard, installer, and pruner
		o.Expect(policy.Spec.PodSelector.MatchExpressions).NotTo(o.BeEmpty(), "should use matchExpressions")
		found := false
		for _, expr := range policy.Spec.PodSelector.MatchExpressions {
			if expr.Key == "app" && expr.Operator == metav1.LabelSelectorOpIn {
				o.Expect(expr.Values).To(o.ContainElement("guard"), "should include guard")
				o.Expect(expr.Values).To(o.ContainElement("installer"), "should include installer")
				o.Expect(expr.Values).To(o.ContainElement("pruner"), "should include pruner")
				g.GinkgoWriter.Printf("  - PodSelector: app in (guard, installer, pruner)\n")
				g.GinkgoWriter.Printf("  - All helper pods can resolve DNS for their operations\n")
				found = true
				break
			}
		}
		o.Expect(found).To(o.BeTrue(), "should have app matchExpression with guard, installer, and pruner")

		g.By("Validating egress destination includes both namespace and pod selectors")
		o.Expect(policy.Spec.Egress).To(o.HaveLen(1), "should have exactly one egress rule")
		egressRule := policy.Spec.Egress[0]
		o.Expect(egressRule.To).To(o.HaveLen(1), "should have exactly one peer in egress rule")
		peer := egressRule.To[0]

		// Validate namespaceSelector
		o.Expect(peer.NamespaceSelector).NotTo(o.BeNil(), "should have namespaceSelector")
		o.Expect(peer.NamespaceSelector.MatchLabels).To(o.HaveKeyWithValue("kubernetes.io/metadata.name", "openshift-dns"))
		g.GinkgoWriter.Printf("  - NamespaceSelector: kubernetes.io/metadata.name=openshift-dns\n")

		// Validate podSelector for DNS daemonset
		o.Expect(peer.PodSelector).NotTo(o.BeNil(), "should have podSelector targeting DNS pods")
		o.Expect(peer.PodSelector.MatchLabels).To(o.HaveKeyWithValue("dns.operator.openshift.io/daemonset-dns", "default"))
		g.GinkgoWriter.Printf("  - PodSelector: dns.operator.openshift.io/daemonset-dns=default\n")
		g.GinkgoWriter.Printf("  - This specifically targets the DNS daemonset pods in openshift-dns namespace\n")

		g.GinkgoWriter.Printf("=== DNS helper policy structure validated successfully ===\n")

		// Test connectivity
		g.GinkgoWriter.Printf("\n=== Testing helper pod DNS connectivity ===\n")
		dnsSvc, err := kubeClient.CoreV1().Services("openshift-dns").Get(ctx, "dns-default", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		dnsIP := dnsSvc.Spec.ClusterIP
		g.GinkgoWriter.Printf("  - DNS service IP: %s\n", dnsIP)

		guardLabels := map[string]string{"app": "guard"}

		g.By("Testing guard pod can access DNS on port 53")
		expectConnectivity(ctx, kubeClient, etcdNamespace, guardLabels, dnsIP, 53, true)

		g.GinkgoWriter.Printf("=== Helper pod DNS connectivity validated successfully ===\n")
	})

	g.It("[Operand][NetworkPolicy][Serial] should restore etcd operand NetworkPolicies after delete or mutation[Timeout:30m]", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.GinkgoWriter.Printf("\n=== Testing Operand NetworkPolicy Reconciliation ===\n")

		g.By("Deleting policies in openshift-etcd and waiting for restoration")
		g.GinkgoWriter.Printf("\n--- Testing Policy Restoration (Delete) ---\n")
		restoreNetworkPolicy(ctx, kubeClient, etcdNamespace, "default-deny")
		restoreNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-guard-to-etcd")
		restoreNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-installer-pruner-to-apiserver")
		restoreNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-operand-helpers-to-dns")
		g.GinkgoWriter.Printf("- All policies successfully restored after deletion\n")

		g.By("Mutating policies in openshift-etcd and waiting for reconciliation")
		g.GinkgoWriter.Printf("\n--- Testing Policy Reconciliation (Mutation) ---\n")
		mutateAndRestoreNetworkPolicy(ctx, kubeClient, etcdNamespace, "default-deny")
		mutateAndRestoreNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-guard-to-etcd")
		mutateAndRestoreNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-installer-pruner-to-apiserver")
		mutateAndRestoreNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-operand-helpers-to-dns")
		g.GinkgoWriter.Printf("- All policies successfully reconciled after mutation\n")
		g.GinkgoWriter.Printf("\n=== Operand NetworkPolicy Reconciliation Test Complete ===\n")
	})

	g.It("[Operator][NetworkPolicy] should enforce allow rules for etcd-operator egress", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.GinkgoWriter.Printf("\n=== Testing etcd-operator Allow Rules ===\n")
		g.GinkgoWriter.Printf("Note: The allow-to-apiserver policy has podSelector {} and egress [{}]\n")
		g.GinkgoWriter.Printf("This allows ALL pods in openshift-etcd-operator to egress to ANY destination\n")
		g.GinkgoWriter.Printf("Therefore deny tests are not possible in this namespace\n")
		g.GinkgoWriter.Printf("We validate policy structure and test allowed connectivity\n\n")

		// Validate policy structure for allow-to-etcd (the most specific policy)
		g.By("Validating allow-to-etcd policy structure")
		policy := getNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-etcd")
		requirePodSelectorLabel(policy, "app", "etcd-operator")
		requireEgressPort(policy, corev1.ProtocolTCP, 2379)
		requireEgressPort(policy, corev1.ProtocolTCP, 9978)
		requireEgressPort(policy, corev1.ProtocolTCP, 9979)
		requireEgressPort(policy, corev1.ProtocolTCP, 9980)
		g.GinkgoWriter.Printf("  - allow-to-etcd: app=etcd-operator → TCP/2379,9978,9979,9980\n")

		// Validate allow-to-dns structure
		g.By("Validating allow-to-dns policy structure")
		dnsPolicy := getNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-dns")
		requireEmptyPodSelector(dnsPolicy)
		requireEgressPort(dnsPolicy, corev1.ProtocolTCP, 53)
		requireEgressPort(dnsPolicy, corev1.ProtocolUDP, 53)
		g.GinkgoWriter.Printf("  - allow-to-dns: all pods → TCP/53, UDP/53, TCP/5353, UDP/5353\n")

		// Validate allow-to-apiserver structure (unrestricted)
		g.By("Validating allow-to-apiserver policy structure")
		apiPolicy := getNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-apiserver")
		requireEmptyPodSelector(apiPolicy)
		requireUnrestrictedEgress(apiPolicy)
		g.GinkgoWriter.Printf("  - allow-to-apiserver: all pods → unrestricted egress\n")

		// Validate allow-to-monitoring structure
		g.By("Validating allow-to-monitoring policy structure")
		monPolicy := getNetworkPolicy(ctx, kubeClient, etcdOperatorNamespace, "allow-to-monitoring")
		requirePodSelectorLabel(monPolicy, "app", "etcd-operator")
		requireEgressPort(monPolicy, corev1.ProtocolTCP, 9091)
		g.GinkgoWriter.Printf("  - allow-to-monitoring: app=etcd-operator → TCP/9091\n")

		// Test allowed connectivity
		g.GinkgoWriter.Printf("\n--- Testing Allowed Connectivity ---\n")

		// Test DNS connectivity
		dnsSvc, err := kubeClient.CoreV1().Services("openshift-dns").Get(ctx, "dns-default", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		dnsIP := dnsSvc.Spec.ClusterIP

		clientLabels := map[string]string{"app": "etcd-operator"}

		g.By("Testing ALLOWED: etcd-operator pod to DNS on port 53")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, dnsIP, 53, true)

		// Test API server connectivity
		kubeSvc, err := kubeClient.CoreV1().Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		apiServerIP := kubeSvc.Spec.ClusterIP

		g.By("Testing ALLOWED: etcd-operator pod to API server on port 443")
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, clientLabels, apiServerIP, 443, true)

		// Test that any pod can reach API server (due to unrestricted allow-to-apiserver)
		g.By("Testing ALLOWED: any pod to API server (unrestricted allow-to-apiserver policy)")
		anyLabels := map[string]string{"app": "test-any-pod"}
		expectConnectivity(ctx, kubeClient, etcdOperatorNamespace, anyLabels, apiServerIP, 443, true)

		g.GinkgoWriter.Printf("\n=== etcd-operator Allow Rules Verified Successfully ===\n")
	})

	g.It("[Operand][NetworkPolicy] should enforce allow rules for etcd operand pods", func() {
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.GinkgoWriter.Printf("\n=== Testing etcd Operand Allow Rules ===\n")
		g.GinkgoWriter.Printf("Note: Deny tests are not possible because broader allow policies\n")
		g.GinkgoWriter.Printf("(e.g. allow-to-apiserver with unrestricted egress) override per-pod restrictions.\n")
		g.GinkgoWriter.Printf("We validate policy structure and test allowed connectivity.\n\n")

		// Validate policy structures
		g.By("Validating allow-guard-to-etcd policy structure")
		guardPolicy := getNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-guard-to-etcd")
		requirePodSelectorLabel(guardPolicy, "app", "guard")
		requireEgressPort(guardPolicy, corev1.ProtocolTCP, 2379)
		requireEgressPort(guardPolicy, corev1.ProtocolTCP, 9980)
		g.GinkgoWriter.Printf("  - allow-guard-to-etcd: app=guard → TCP/2379, TCP/9980\n")

		g.By("Validating allow-installer-pruner-to-apiserver policy structure")
		ipPolicy := getNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-installer-pruner-to-apiserver")

		// Log the actual policy egress rules for debugging
		g.GinkgoWriter.Printf("  - Policy egress rules: %d\n", len(ipPolicy.Spec.Egress))
		for i, rule := range ipPolicy.Spec.Egress {
			g.GinkgoWriter.Printf("    Rule %d: ports=%d, to=%d\n", i, len(rule.Ports), len(rule.To))
			for _, p := range rule.Ports {
				if p.Port != nil && p.Protocol != nil {
					g.GinkgoWriter.Printf("      Port: %s/%d\n", *p.Protocol, p.Port.IntValue())
				} else if p.Port != nil {
					g.GinkgoWriter.Printf("      Port: TCP/%d (default)\n", p.Port.IntValue())
				}
			}
			if len(rule.Ports) == 0 && len(rule.To) == 0 {
				g.GinkgoWriter.Printf("      Unrestricted egress (all ports, all destinations)\n")
			}
		}

		// Check if it has specific port 6443 or unrestricted egress
		if hasPortInEgress(ipPolicy.Spec.Egress, corev1.ProtocolTCP, 6443) {
			g.GinkgoWriter.Printf("  - Egress: TCP/6443 (API server)\n")
		} else if len(ipPolicy.Spec.Egress) > 0 && len(ipPolicy.Spec.Egress[0].Ports) == 0 {
			g.GinkgoWriter.Printf("  - Egress: unrestricted (allows all destinations/ports)\n")
		} else {
			g.Fail(fmt.Sprintf("allow-installer-pruner-to-apiserver: expected egress port TCP/6443 or unrestricted egress, got %v", ipPolicy.Spec.Egress))
		}

		// Validate pod selector
		if len(ipPolicy.Spec.PodSelector.MatchExpressions) > 0 {
			found := false
			for _, expr := range ipPolicy.Spec.PodSelector.MatchExpressions {
				if expr.Key == "app" && expr.Operator == metav1.LabelSelectorOpIn {
					o.Expect(expr.Values).To(o.ContainElement("installer"))
					o.Expect(expr.Values).To(o.ContainElement("pruner"))
					g.GinkgoWriter.Printf("  - PodSelector: app in (installer, pruner)\n")
					found = true
					break
				}
			}
			o.Expect(found).To(o.BeTrue(), "should have app matchExpression with installer and pruner")
		} else if len(ipPolicy.Spec.PodSelector.MatchLabels) == 0 {
			g.GinkgoWriter.Printf("  - PodSelector: {} (applies to all pods)\n")
		} else {
			g.GinkgoWriter.Printf("  - PodSelector matchLabels: %v\n", ipPolicy.Spec.PodSelector.MatchLabels)
		}
		g.GinkgoWriter.Printf("  - allow-installer-pruner-to-apiserver validated\n")

		g.By("Validating allow-operand-helpers-to-dns policy structure")
		dnsPolicy := getNetworkPolicy(ctx, kubeClient, etcdNamespace, "allow-operand-helpers-to-dns")
		requireEgressPort(dnsPolicy, corev1.ProtocolTCP, 53)
		requireEgressPort(dnsPolicy, corev1.ProtocolUDP, 53)
		g.GinkgoWriter.Printf("  - allow-operand-helpers-to-dns: app in (guard, installer, pruner) → DNS ports\n")

		// Test allowed connectivity
		g.GinkgoWriter.Printf("\n--- Testing Allowed Connectivity ---\n")

		// Test 1: Guard pod egress to etcd health endpoint
		etcdSvc, err := kubeClient.CoreV1().Services(etcdNamespace).Get(ctx, "etcd", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		etcdIP := etcdSvc.Spec.ClusterIP
		g.GinkgoWriter.Printf("  - etcd service IP: %s\n", etcdIP)

		guardLabels := map[string]string{"app": "guard"}
		g.By("Testing ALLOWED: guard pod to etcd health port 9980")
		expectConnectivity(ctx, kubeClient, etcdNamespace, guardLabels, etcdIP, 9980, true)

		// Test 2: Installer/Pruner pod egress to API server
		g.GinkgoWriter.Printf("\n  Note: Installer/Pruner pods connect to actual API server endpoint (not kubernetes service)\n")
		g.GinkgoWriter.Printf("  Policy allows egress on port 6443 to reach the API server\n")
		g.GinkgoWriter.Printf("  Cannot test connectivity via kubernetes service ClusterIP (listens on port 443, not 6443)\n")
		g.GinkgoWriter.Printf("  Policy structure validation confirms correct configuration\n")

		// Test 3: Helper pod egress to DNS
		dnsSvc, err := kubeClient.CoreV1().Services("openshift-dns").Get(ctx, "dns-default", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		dnsIP := dnsSvc.Spec.ClusterIP
		g.GinkgoWriter.Printf("  - DNS service IP: %s\n", dnsIP)

		g.By("Testing ALLOWED: guard pod to DNS port 53")
		expectConnectivity(ctx, kubeClient, etcdNamespace, guardLabels, dnsIP, 53, true)

		installerLabels := map[string]string{"app": "installer"}
		g.By("Testing ALLOWED: installer pod to DNS port 53")
		expectConnectivity(ctx, kubeClient, etcdNamespace, installerLabels, dnsIP, 53, true)

		prunerLabels := map[string]string{"app": "pruner"}
		g.By("Testing ALLOWED: pruner pod to DNS port 53")
		expectConnectivity(ctx, kubeClient, etcdNamespace, prunerLabels, dnsIP, 53, true)

		g.GinkgoWriter.Printf("\n=== All Operand Allow Rules Verified Successfully ===\n")
	})
})

func validateEtcdOperatorPolicies(ctx context.Context, client kubernetes.Interface) {
	g.GinkgoWriter.Printf("\n=== Validating allow-to-dns policy ===\n")
	policy := getNetworkPolicy(ctx, client, etcdOperatorNamespace, "allow-to-dns")
	g.GinkgoWriter.Printf("  - Policy found: %s/%s\n", policy.Namespace, policy.Name)
	requireEmptyPodSelector(policy)
	g.GinkgoWriter.Printf("  - PodSelector: {} (applies to all pods)\n")
	requireEgressPort(policy, corev1.ProtocolTCP, 53)
	requireEgressPort(policy, corev1.ProtocolUDP, 53)
	requireEgressPort(policy, corev1.ProtocolTCP, 5353)
	requireEgressPort(policy, corev1.ProtocolUDP, 5353)
	g.GinkgoWriter.Printf("  - Egress ports: TCP/53, UDP/53, TCP/5353, UDP/5353\n")

	g.GinkgoWriter.Printf("\n=== Validating allow-to-apiserver policy ===\n")
	policy = getNetworkPolicy(ctx, client, etcdOperatorNamespace, "allow-to-apiserver")
	g.GinkgoWriter.Printf("  - Policy found: %s/%s\n", policy.Namespace, policy.Name)
	requireEmptyPodSelector(policy)
	g.GinkgoWriter.Printf("  - PodSelector: {} (applies to all pods)\n")
	requireUnrestrictedEgress(policy)
	g.GinkgoWriter.Printf("  - Egress: unrestricted (allows all destinations)\n")

	g.GinkgoWriter.Printf("\n=== Validating allow-to-etcd policy ===\n")
	policy = getNetworkPolicy(ctx, client, etcdOperatorNamespace, "allow-to-etcd")
	g.GinkgoWriter.Printf("  - Policy found: %s/%s\n", policy.Namespace, policy.Name)
	requirePodSelectorLabel(policy, "app", "etcd-operator")
	g.GinkgoWriter.Printf("  - PodSelector: app=etcd-operator\n")
	requireEgressPort(policy, corev1.ProtocolTCP, 2379)
	requireEgressPort(policy, corev1.ProtocolTCP, 9978)
	requireEgressPort(policy, corev1.ProtocolTCP, 9979)
	requireEgressPort(policy, corev1.ProtocolTCP, 9980)
	g.GinkgoWriter.Printf("  - Egress ports: TCP/2379 (etcd client), TCP/9978 (metrics server), TCP/9979 (metrics proxy), TCP/9980 (health check)\n")

	g.GinkgoWriter.Printf("\n=== Validating allow-to-monitoring policy ===\n")
	policy = getNetworkPolicy(ctx, client, etcdOperatorNamespace, "allow-to-monitoring")
	g.GinkgoWriter.Printf("  - Policy found: %s/%s\n", policy.Namespace, policy.Name)
	requirePodSelectorLabel(policy, "app", "etcd-operator")
	g.GinkgoWriter.Printf("  - PodSelector: app=etcd-operator\n")
	requireEgressPort(policy, corev1.ProtocolTCP, 9091)
	g.GinkgoWriter.Printf("  - Egress port: TCP/9091 (Thanos/Prometheus)\n")

	g.GinkgoWriter.Printf("\n=== Validating allow-to-metrics policy ===\n")
	policy = getNetworkPolicy(ctx, client, etcdOperatorNamespace, "allow-to-metrics")
	g.GinkgoWriter.Printf("  - Policy found: %s/%s\n", policy.Namespace, policy.Name)
	requirePodSelectorLabel(policy, "app", "etcd-operator")
	g.GinkgoWriter.Printf("  - PodSelector: app=etcd-operator\n")
	requireIngressPort(policy, corev1.ProtocolTCP, 8443)
	g.GinkgoWriter.Printf("  - Ingress port: TCP/8443 (from any source)\n")
	g.GinkgoWriter.Printf("\n=== All etcd-operator policies validated successfully ===\n")
}

func validateEtcdNamespacePolicies(ctx context.Context, client kubernetes.Interface) {
	policies, err := client.NetworkingV1().NetworkPolicies(etcdNamespace).List(ctx, metav1.ListOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(policies.Items).NotTo(o.BeEmpty())
	logPolicyNames(etcdNamespace, policies.Items)

	g.GinkgoWriter.Printf("\n=== Validating %s namespace policies ===\n", etcdNamespace)

	g.GinkgoWriter.Printf("  Checking for default-deny policy...\n")
	o.Expect(hasDefaultDeny(policies.Items)).To(o.BeTrue(), "expected a default deny-all policy in %s", etcdNamespace)
	g.GinkgoWriter.Printf("  - default-deny policy found\n")

	g.GinkgoWriter.Printf("  Checking for API server egress (TCP/6443 or unrestricted)...\n")
	hasAPIServerEgress := hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 6443) || hasUnrestrictedEgressInNamespace(policies.Items)
	o.Expect(hasAPIServerEgress).To(o.BeTrue(), "expected an egress policy allowing TCP/6443 or unrestricted egress in %s", etcdNamespace)
	if hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 6443) {
		g.GinkgoWriter.Printf("  - TCP/6443 egress allowed (installer/pruner to API server)\n")
	} else {
		g.GinkgoWriter.Printf("  - Unrestricted egress found (installer/pruner to API server)\n")
	}

	g.GinkgoWriter.Printf("  Checking for etcd egress (TCP/2379 or TCP/9980)...\n")
	if !hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 2379) && !hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 9980) {
		g.Fail(fmt.Sprintf("expected an egress policy allowing TCP/2379 or TCP/9980 in %s", etcdNamespace))
	}
	has2379 := hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 2379)
	has9980 := hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 9980)
	if has2379 && has9980 {
		g.GinkgoWriter.Printf("  - TCP/2379 and TCP/9980 egress allowed (guard to etcd)\n")
	} else if has2379 {
		g.GinkgoWriter.Printf("  - TCP/2379 egress allowed\n")
	} else {
		g.GinkgoWriter.Printf("  - TCP/9980 egress allowed\n")
	}

	g.GinkgoWriter.Printf("  Checking for DNS egress (TCP/UDP 53 or 5353)...\n")
	if !(hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 53) || hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 5353) ||
		hasEgressPortInNamespace(policies.Items, corev1.ProtocolUDP, 53) || hasEgressPortInNamespace(policies.Items, corev1.ProtocolUDP, 5353)) {
		g.Fail(fmt.Sprintf("expected an egress policy allowing DNS ports in %s", etcdNamespace))
	}
	dnsPortsFound := []string{}
	if hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 53) {
		dnsPortsFound = append(dnsPortsFound, "TCP/53")
	}
	if hasEgressPortInNamespace(policies.Items, corev1.ProtocolUDP, 53) {
		dnsPortsFound = append(dnsPortsFound, "UDP/53")
	}
	if hasEgressPortInNamespace(policies.Items, corev1.ProtocolTCP, 5353) {
		dnsPortsFound = append(dnsPortsFound, "TCP/5353")
	}
	if hasEgressPortInNamespace(policies.Items, corev1.ProtocolUDP, 5353) {
		dnsPortsFound = append(dnsPortsFound, "UDP/5353")
	}
	g.GinkgoWriter.Printf("  - DNS egress allowed on ports: %v\n", dnsPortsFound)
	g.GinkgoWriter.Printf("\n=== All %s namespace policies validated successfully ===\n", etcdNamespace)
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

func requireEmptyPodSelector(policy *networkingv1.NetworkPolicy) {
	g.GinkgoHelper()
	if len(policy.Spec.PodSelector.MatchLabels) != 0 || len(policy.Spec.PodSelector.MatchExpressions) != 0 {
		g.Fail(fmt.Sprintf("%s/%s: expected empty podSelector, got matchLabels=%v matchExpressions=%v",
			policy.Namespace, policy.Name, policy.Spec.PodSelector.MatchLabels, policy.Spec.PodSelector.MatchExpressions))
	}
}

func requireUnrestrictedEgress(policy *networkingv1.NetworkPolicy) {
	g.GinkgoHelper()
	if len(policy.Spec.Egress) != 1 {
		g.Fail(fmt.Sprintf("%s/%s: expected exactly one egress rule for unrestricted egress, got %d rules",
			policy.Namespace, policy.Name, len(policy.Spec.Egress)))
	}
	egressRule := policy.Spec.Egress[0]
	if len(egressRule.Ports) != 0 || len(egressRule.To) != 0 {
		g.Fail(fmt.Sprintf("%s/%s: expected unrestricted egress rule (no ports, no to), got ports=%v to=%v",
			policy.Namespace, policy.Name, egressRule.Ports, egressRule.To))
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

func hasUnrestrictedEgressInNamespace(policies []networkingv1.NetworkPolicy) bool {
	for _, policy := range policies {
		for _, rule := range policy.Spec.Egress {
			if len(rule.Ports) == 0 && len(rule.To) == 0 {
				return true
			}
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
	g.GinkgoWriter.Printf("  Deleting NetworkPolicy %s/%s...\n", namespace, name)
	o.Expect(client.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, name, metav1.DeleteOptions{})).NotTo(o.HaveOccurred())
	g.GinkgoWriter.Printf("  - NetworkPolicy deleted, waiting for operator to restore it...\n")
	err := wait.PollImmediate(5*time.Second, 30*time.Minute, func() (bool, error) {
		_, err := client.NetworkingV1().NetworkPolicies(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return true, nil
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for NetworkPolicy %s/%s to be restored", namespace, name)
	g.GinkgoWriter.Printf("  - NetworkPolicy %s/%s successfully restored by operator\n", namespace, name)
}

func mutateAndRestoreNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, name string) {
	g.GinkgoHelper()
	g.GinkgoWriter.Printf("  Mutating NetworkPolicy %s/%s...\n", namespace, name)
	original := getNetworkPolicy(ctx, client, namespace, name)
	patch := []byte(`{"spec":{"podSelector":{"matchLabels":{"np-reconcile":"mutated"}}}}`)
	_, err := client.NetworkingV1().NetworkPolicies(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())
	g.GinkgoWriter.Printf("  - NetworkPolicy mutated with np-reconcile=mutated label, waiting for operator to reconcile...\n")

	err = wait.PollImmediate(5*time.Second, 30*time.Minute, func() (bool, error) {
		current := getNetworkPolicy(ctx, client, namespace, name)
		return current.Spec.PodSelector.MatchLabels["np-reconcile"] == "" &&
			current.Spec.PodSelector.MatchLabels["np-reconcile"] == original.Spec.PodSelector.MatchLabels["np-reconcile"], nil
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "timed out waiting for NetworkPolicy %s/%s spec to be restored", namespace, name)
	g.GinkgoWriter.Printf("  - NetworkPolicy %s/%s successfully reconciled to original state\n", namespace, name)
}

func expectConnectivity(ctx context.Context, client kubernetes.Interface, namespace string, labels map[string]string, host string, port int32, shouldSucceed bool) {
	g.GinkgoHelper()
	expectedResult := "ALLOWED"
	if !shouldSucceed {
		expectedResult = "DENIED"
	}
	g.GinkgoWriter.Printf("  Testing connectivity: %s (expected: %s)\n", formatHostPort(host, port), expectedResult)
	err := wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		ok, err := runConnectivityCheck(ctx, client, namespace, labels, host, port)
		if err != nil {
			g.GinkgoWriter.Printf("    Connectivity check error: %v (retrying...)\n", err)
			return false, err
		}
		return ok == shouldSucceed, nil
	})
	o.Expect(err).NotTo(o.HaveOccurred(), "connectivity to %s should be %t", formatHostPort(host, port), shouldSucceed)
	actualResult := "ALLOWED"
	if !shouldSucceed {
		actualResult = "DENIED"
	}
	g.GinkgoWriter.Printf("  - Connectivity test passed: %s is %s\n", formatHostPort(host, port), actualResult)
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
						formatHostPort(host, port),
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
		g.GinkgoWriter.Printf("    Pod %s/%s failed to complete: %v\n", namespace, name, err)
		return false, err
	}
	completed, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		g.GinkgoWriter.Printf("    Failed to get pod %s/%s: %v\n", namespace, name, err)
		return false, err
	}
	if len(completed.Status.ContainerStatuses) == 0 {
		g.GinkgoWriter.Printf("    Pod %s/%s has no container status\n", namespace, name)
		return false, fmt.Errorf("no container status recorded for pod %s", name)
	}
	exitCode := completed.Status.ContainerStatuses[0].State.Terminated.ExitCode
	g.GinkgoWriter.Printf("    client pod %s/%s exitCode=%d phase=%s\n", namespace, name, exitCode, completed.Status.Phase)

	// Log pod events and status if failed
	if exitCode != 0 {
		if completed.Status.ContainerStatuses[0].State.Terminated != nil {
			g.GinkgoWriter.Printf("    Termination reason: %s\n", completed.Status.ContainerStatuses[0].State.Terminated.Reason)
			g.GinkgoWriter.Printf("    Termination message: %s\n", completed.Status.ContainerStatuses[0].State.Terminated.Message)
		}

		// Get pod logs
		logOpts := &corev1.PodLogOptions{}
		req := client.CoreV1().Pods(namespace).GetLogs(name, logOpts)
		logs, err := req.DoRaw(ctx)
		if err == nil {
			g.GinkgoWriter.Printf("    Pod logs:\n%s\n", string(logs))
		}
	}

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
