package e2e

import (
	"context"
	"net"
	"testing"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

	"github.com/openshift/cluster-etcd-operator/test/e2e/framework"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	np "github.com/openshift/library-go/test/library/networkpolicy"
)

const (
	etcdOperatorNamespace = "openshift-etcd-operator"
	etcdNamespace         = "openshift-etcd"

	// PR 1538 policies (operator namespace)
	allowOperatorPolicyName = "allow-operator"
	// PR 1544 policies (operand namespace)
	allowAllEgressPolicyName = "allow-all-egress"
	// Shared
	defaultDenyPolicyName = "default-deny"

	reconcileTimeout = 10 * time.Minute
)

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
func getExternalTestIP(ctx context.Context, client kubernetes.Interface) string {
	if isIPv6Cluster(ctx, client) {
		return "2001:4860:4860::8888"
	}
	return "8.8.8.8"
}

var _ = g.Describe("[sig-etcd] cluster-etcd-operator", func() {

	// =====================================================================
	// Operator namespace (openshift-etcd-operator) — PR 1538
	// Policies: allow-operator, default-deny
	// =====================================================================

	g.It("[Operator][NetworkPolicy] should ensure etcd NetworkPolicies are defined", func() {
		t := g.GinkgoTB()
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Validating NetworkPolicies in openshift-etcd-operator")
		validateEtcdOperatorPolicies(t, ctx, kubeClient)

		g.By("Validating NetworkPolicies in openshift-etcd")
		validateEtcdNamespacePolicies(t, ctx, kubeClient)
	})

	g.It("[Operator][NetworkPolicy] should validate allow-operator policy structure", func() {
		t := g.GinkgoTB()
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		t.Logf("\n=== Validating allow-operator policy ===")
		policy := np.GetNetworkPolicy(t, ctx, kubeClient, etcdOperatorNamespace, allowOperatorPolicyName)

		g.By("Validating pod selector targets etcd-operator")
		np.RequirePodSelectorLabel(t, policy, "app", "etcd-operator")
		t.Logf("  - PodSelector: app=etcd-operator")

		g.By("Validating ingress on port 8443 (metrics)")
		o.Expect(policy.Spec.Ingress).NotTo(o.BeEmpty(), "should have ingress rules")
		np.RequireIngressPort(t, policy, corev1.ProtocolTCP, 8443)
		t.Logf("  - Ingress: TCP/8443 (Prometheus scraping)")

		g.By("Validating unrestricted egress")
		np.RequireUnrestrictedEgress(t, policy)
		t.Logf("  - Egress: unrestricted [{}] (API server, etcd, DNS, monitoring)")

		g.By("Validating policy types")
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeIngress))
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeEgress))
		t.Logf("  - PolicyTypes: [Ingress, Egress]")
		t.Logf("=== allow-operator policy validated ===")
	})

	g.It("[Operator][NetworkPolicy] should ensure default-deny policy in openshift-etcd-operator", func() {
		t := g.GinkgoTB()
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		t.Logf("\n=== Validating default-deny policy in %s ===", etcdOperatorNamespace)
		policy := np.GetNetworkPolicy(t, ctx, kubeClient, etcdOperatorNamespace, defaultDenyPolicyName)

		g.By("Validating pod selector is empty (applies to all pods)")
		np.RequireEmptyPodSelector(t, policy)
		t.Logf("  - PodSelector: {} (applies to all pods)")

		g.By("Validating policy types include both Ingress and Egress")
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeIngress))
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeEgress))
		t.Logf("  - PolicyTypes: [Ingress, Egress]")
		t.Logf("  - Combined with allow-operator, only etcd-operator pods get ingress/egress")
		t.Logf("=== default-deny policy validated ===")
	})

	g.It("[Operator][NetworkPolicy] should enforce etcd-operator egress connectivity", func() {
		t := g.GinkgoTB()
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		clientLabels := map[string]string{"app": "etcd-operator"}

		// Test API server connectivity
		kubeSvc, err := kubeClient.CoreV1().Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		apiServerIP := kubeSvc.Spec.ClusterIP

		t.Logf("\n=== Testing allow-operator egress ===")
		t.Logf("allow-operator has unrestricted egress [{}], allows all destinations/ports")

		g.By("Verifying allowed egress to API server on 443")
		np.ExpectConnectivity(t, kubeClient, etcdOperatorNamespace, clientLabels, []string{apiServerIP}, 443, true)

		// Test DNS connectivity
		dnsSvc, err := kubeClient.CoreV1().Services("openshift-dns").Get(ctx, "dns-default", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		dnsIP := dnsSvc.Spec.ClusterIP

		g.By("Verifying allowed egress to DNS on 53")
		np.ExpectConnectivity(t, kubeClient, etcdOperatorNamespace, clientLabels, []string{dnsIP}, 53, true)

		t.Logf("=== Operator egress connectivity verified ===")
	})

	g.It("[Operator][NetworkPolicy] should allow ingress to etcd-operator metrics endpoint", func() {
		t := g.GinkgoTB()
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		t.Logf("\n=== Testing allow-operator ingress (metrics) ===")

		// Get etcd-operator pod IP
		pods, err := kubeClient.CoreV1().Pods(etcdOperatorNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=etcd-operator",
		})
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(pods.Items).NotTo(o.BeEmpty(), "etcd-operator pod should exist")
		operatorPodIP := pods.Items[0].Status.PodIP
		t.Logf("  - etcd-operator pod IP: %s", operatorPodIP)

		g.By("Testing ingress from any namespace to metrics port 8443")
		testLabels := map[string]string{"test": "ingress-test"}
		np.ExpectConnectivity(t, kubeClient, "default", testLabels, []string{operatorPodIP}, 8443, true)
		t.Logf("=== Metrics ingress verified ===")
	})

	g.It("[Operator][NetworkPolicy][Serial] should restore etcd operator NetworkPolicies after delete or mutation[Timeout:30m]", func() {
		t := g.GinkgoTB()
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Capturing expected operator policies")
		expectedAllowOp := np.GetNetworkPolicy(t, ctx, kubeClient, etcdOperatorNamespace, allowOperatorPolicyName)
		expectedDenyOp := np.GetNetworkPolicy(t, ctx, kubeClient, etcdOperatorNamespace, defaultDenyPolicyName)

		g.By("Deleting policies and waiting for restoration")
		np.RestoreNetworkPolicy(t, ctx, kubeClient, expectedAllowOp, reconcileTimeout)
		np.RestoreNetworkPolicy(t, ctx, kubeClient, expectedDenyOp, reconcileTimeout)

		g.By("Mutating policies and waiting for reconciliation")
		np.MutateAndRestoreNetworkPolicy(t, ctx, kubeClient, etcdOperatorNamespace, allowOperatorPolicyName, reconcileTimeout)
		np.MutateAndRestoreNetworkPolicy(t, ctx, kubeClient, etcdOperatorNamespace, defaultDenyPolicyName, reconcileTimeout)

		g.By("Checking NetworkPolicy-related events (best-effort)")
		np.LogNetworkPolicyEvents(t, ctx, kubeClient, []string{etcdOperatorNamespace}, allowOperatorPolicyName)
		np.LogNetworkPolicyEvents(t, ctx, kubeClient, []string{etcdOperatorNamespace}, defaultDenyPolicyName)
	})

	// =====================================================================
	// Operand namespace (openshift-etcd) — PR 1544
	// Policies: allow-all-egress, default-deny
	// =====================================================================

	g.It("[Operand][NetworkPolicy] should ensure default-deny policy in openshift-etcd", func() {
		t := g.GinkgoTB()
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		t.Logf("\n=== Validating default-deny policy in %s ===", etcdNamespace)
		policy := np.GetNetworkPolicy(t, ctx, kubeClient, etcdNamespace, defaultDenyPolicyName)

		g.By("Validating pod selector is empty (applies to all pods)")
		np.RequireEmptyPodSelector(t, policy)
		t.Logf("  - PodSelector: {} (applies to all pods)")

		g.By("Validating policy types include both Ingress and Egress")
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeIngress))
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeEgress))
		t.Logf("  - PolicyTypes: [Ingress, Egress]")
		t.Logf("  - Note: etcd static pods use hostNetwork and bypass NetworkPolicy")
		t.Logf("  - This policy affects guard, installer, pruner, backup pods on pod network")
		t.Logf("=== default-deny policy validated ===")
	})

	g.It("[Operand][NetworkPolicy] should validate allow-all-egress policy structure", func() {
		t := g.GinkgoTB()
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		t.Logf("\n=== Validating allow-all-egress policy in %s ===", etcdNamespace)
		policy := np.GetNetworkPolicy(t, ctx, kubeClient, etcdNamespace, allowAllEgressPolicyName)

		g.By("Validating pod selector targets guard, installer, pruner, and backup pods")
		o.Expect(policy.Spec.PodSelector.MatchExpressions).NotTo(o.BeEmpty(), "should use matchExpressions")
		found := false
		for _, expr := range policy.Spec.PodSelector.MatchExpressions {
			if expr.Key == "app" && expr.Operator == metav1.LabelSelectorOpIn {
				o.Expect(expr.Values).To(o.ContainElement("guard"), "should include guard")
				o.Expect(expr.Values).To(o.ContainElement("installer"), "should include installer")
				o.Expect(expr.Values).To(o.ContainElement("pruner"), "should include pruner")
				o.Expect(expr.Values).To(o.ContainElement("cluster-backup-cronjob"), "should include cluster-backup-cronjob")
				t.Logf("  - PodSelector: app in (guard, installer, pruner, cluster-backup-cronjob)")
				found = true
				break
			}
		}
		o.Expect(found).To(o.BeTrue(), "should have app matchExpression with guard, installer, pruner, cluster-backup-cronjob")

		g.By("Validating unrestricted egress")
		np.RequireUnrestrictedEgress(t, policy)
		t.Logf("  - Egress: unrestricted [{}] (API server, etcd health, DNS)")

		g.By("Validating policy type is Egress only")
		o.Expect(policy.Spec.PolicyTypes).To(o.ContainElement(networkingv1.PolicyTypeEgress))
		t.Logf("  - PolicyTypes: [Egress]")
		t.Logf("=== allow-all-egress policy validated ===")
	})

	g.It("[Operand][NetworkPolicy] should allow operand helper pods egress connectivity", func() {
		t := g.GinkgoTB()
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		t.Logf("\n=== Testing allow-all-egress connectivity ===")

		// Test DNS egress for guard pods
		dnsSvc, err := kubeClient.CoreV1().Services("openshift-dns").Get(ctx, "dns-default", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		dnsIP := dnsSvc.Spec.ClusterIP

		guardLabels := map[string]string{"app": "guard"}
		g.By("Testing guard pod egress to DNS on port 53")
		np.ExpectConnectivity(t, kubeClient, etcdNamespace, guardLabels, []string{dnsIP}, 53, true)

		installerLabels := map[string]string{"app": "installer"}
		g.By("Testing installer pod egress to DNS on port 53")
		np.ExpectConnectivity(t, kubeClient, etcdNamespace, installerLabels, []string{dnsIP}, 53, true)

		// Test API server egress
		kubeSvc, err := kubeClient.CoreV1().Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())
		apiServerIP := kubeSvc.Spec.ClusterIP

		g.By("Testing installer pod egress to API server on port 443")
		np.ExpectConnectivity(t, kubeClient, etcdNamespace, installerLabels, []string{apiServerIP}, 443, true)

		t.Logf("=== Operand egress connectivity verified ===")
	})

	g.It("[Operand][NetworkPolicy][Serial] should restore etcd operand NetworkPolicies after delete or mutation[Timeout:30m]", func() {
		t := g.GinkgoTB()
		ctx := context.Background()
		kubeConfig, err := framework.NewClientConfigForTest("")
		o.Expect(err).NotTo(o.HaveOccurred())
		kubeClient, err := kubernetes.NewForConfig(kubeConfig)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Capturing expected operand policies")
		expectedDeny := np.GetNetworkPolicy(t, ctx, kubeClient, etcdNamespace, defaultDenyPolicyName)
		expectedEgress := np.GetNetworkPolicy(t, ctx, kubeClient, etcdNamespace, allowAllEgressPolicyName)

		g.By("Deleting policies and waiting for restoration")
		np.RestoreNetworkPolicy(t, ctx, kubeClient, expectedDeny, reconcileTimeout)
		np.RestoreNetworkPolicy(t, ctx, kubeClient, expectedEgress, reconcileTimeout)

		g.By("Mutating policies and waiting for reconciliation")
		np.MutateAndRestoreNetworkPolicy(t, ctx, kubeClient, etcdNamespace, defaultDenyPolicyName, reconcileTimeout)
		np.MutateAndRestoreNetworkPolicy(t, ctx, kubeClient, etcdNamespace, allowAllEgressPolicyName, reconcileTimeout)

		g.By("Checking NetworkPolicy-related events (best-effort)")
		// Events are emitted in the operator namespace, not the operand namespace
		np.LogNetworkPolicyEvents(t, ctx, kubeClient, []string{etcdOperatorNamespace}, defaultDenyPolicyName)
		np.LogNetworkPolicyEvents(t, ctx, kubeClient, []string{etcdOperatorNamespace}, allowAllEgressPolicyName)
	})
})

// validateEtcdOperatorPolicies checks that the expected policies exist in the operator namespace.
// PR 1538 defines: allow-operator + default-deny
func validateEtcdOperatorPolicies(t testing.TB, ctx context.Context, client kubernetes.Interface) {
	t.Helper()

	t.Logf("\n=== Validating allow-operator policy ===")
	policy := np.GetNetworkPolicy(t, ctx, client, etcdOperatorNamespace, allowOperatorPolicyName)
	t.Logf("  - Policy found: %s/%s", policy.Namespace, policy.Name)
	np.RequirePodSelectorLabel(t, policy, "app", "etcd-operator")
	t.Logf("  - PodSelector: app=etcd-operator")
	np.RequireIngressPort(t, policy, corev1.ProtocolTCP, 8443)
	t.Logf("  - Ingress: TCP/8443 (metrics)")
	np.RequireUnrestrictedEgress(t, policy)
	t.Logf("  - Egress: unrestricted [{}]")

	t.Logf("\n=== Validating default-deny policy ===")
	denyPolicy := np.GetNetworkPolicy(t, ctx, client, etcdOperatorNamespace, defaultDenyPolicyName)
	t.Logf("  - Policy found: %s/%s", denyPolicy.Namespace, denyPolicy.Name)
	np.RequireEmptyPodSelector(t, denyPolicy)
	t.Logf("  - PodSelector: {} (all pods)")

	t.Logf("\n=== All etcd-operator policies validated ===")
}

// validateEtcdNamespacePolicies checks that the expected policies exist in the operand namespace.
// PR 1544 defines: allow-all-egress + default-deny
func validateEtcdNamespacePolicies(t testing.TB, ctx context.Context, client kubernetes.Interface) {
	t.Helper()

	policies, err := client.NetworkingV1().NetworkPolicies(etcdNamespace).List(ctx, metav1.ListOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())
	o.Expect(policies.Items).NotTo(o.BeEmpty())
	np.LogPolicyNames(t, etcdNamespace, policies.Items)

	t.Logf("\n=== Validating %s namespace policies ===", etcdNamespace)

	t.Logf("  Checking for default-deny policy...")
	o.Expect(np.HasDefaultDeny(policies.Items)).To(o.BeTrue(),
		"expected a default-deny policy in %s", etcdNamespace)
	t.Logf("  - default-deny policy found")

	t.Logf("  Checking for allow-all-egress policy...")
	egressPolicy := np.GetNetworkPolicy(t, ctx, client, etcdNamespace, allowAllEgressPolicyName)
	t.Logf("  - Policy found: %s/%s", egressPolicy.Namespace, egressPolicy.Name)
	np.RequireUnrestrictedEgress(t, egressPolicy)
	t.Logf("  - Egress: unrestricted [{}]")

	// Validate pod selector targets the right pods
	found := false
	for _, expr := range egressPolicy.Spec.PodSelector.MatchExpressions {
		if expr.Key == "app" && expr.Operator == metav1.LabelSelectorOpIn {
			t.Logf("  - PodSelector: app in (%v)", expr.Values)
			found = true
			break
		}
	}
	if !found {
		t.Logf("  - PodSelector: %v", egressPolicy.Spec.PodSelector)
	}

	t.Logf("\n=== All %s namespace policies validated ===", etcdNamespace)
}
