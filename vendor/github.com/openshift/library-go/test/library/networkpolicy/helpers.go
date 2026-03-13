// Package networkpolicy provides reusable helpers for testing Kubernetes
// NetworkPolicy enforcement.  The helpers are framework-agnostic: they accept
// a testing.TB for logging and return errors so callers can use any test
// framework (standard Go testing, Ginkgo, etc.).
package networkpolicy

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	// DefaultAgnhostImage is the default agnhost image used for test pods.
	DefaultAgnhostImage = "registry.k8s.io/e2e-test-images/agnhost:2.45"
)

// ----- IP helpers -----

// IsIPv6 returns true if the given IP string is an IPv6 address.
func IsIPv6(ip string) bool {
	return net.ParseIP(ip) != nil && strings.Contains(ip, ":")
}

// FormatIPPort formats an IP:port pair, using brackets for IPv6 addresses
// (e.g. "[::1]:8443").
func FormatIPPort(ip string, port int32) string {
	if IsIPv6(ip) {
		return fmt.Sprintf("[%s]:%d", ip, port)
	}
	return fmt.Sprintf("%s:%d", ip, port)
}

// PodIPs returns all IP addresses assigned to a pod (dual-stack aware).
func PodIPs(pod *corev1.Pod) []string {
	var ips []string
	for _, podIP := range pod.Status.PodIPs {
		if podIP.IP != "" {
			ips = append(ips, podIP.IP)
		}
	}
	if len(ips) == 0 && pod.Status.PodIP != "" {
		ips = append(ips, pod.Status.PodIP)
	}
	return ips
}

// ServiceClusterIPs returns all ClusterIPs for a service (dual-stack aware).
func ServiceClusterIPs(svc *corev1.Service) []string {
	if len(svc.Spec.ClusterIPs) > 0 {
		return svc.Spec.ClusterIPs
	}
	if svc.Spec.ClusterIP != "" {
		return []string{svc.Spec.ClusterIP}
	}
	return nil
}

// ----- Pod construction -----

// NetexecPod returns a Pod object running agnhost netexec on the given port.
func NetexecPod(name, namespace string, labels map[string]string, port int32) *corev1.Pod {
	return NetexecPodWithImage(name, namespace, labels, port, DefaultAgnhostImage)
}

// NetexecPodWithImage returns a Pod object running agnhost netexec with a
// custom image.
func NetexecPodWithImage(name, namespace string, labels map[string]string, port int32, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   boolptr(true),
				RunAsUser:      int64ptr(1001),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{
				{
					Name:  "netexec",
					Image: image,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: boolptr(false),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						RunAsNonRoot:             boolptr(true),
						RunAsUser:                int64ptr(1001),
					},
					Command: []string{"/agnhost"},
					Args:    []string{"netexec", fmt.Sprintf("--http-port=%d", port)},
					Ports: []corev1.ContainerPort{
						{ContainerPort: port},
					},
				},
			},
		},
	}
}

// CreateServerPod creates an agnhost netexec server pod in the given namespace,
// waits for it to be Ready, and returns all its PodIPs along with a cleanup
// function.
func CreateServerPod(t testing.TB, kubeClient kubernetes.Interface, namespace, name string, labels map[string]string, port int32) ([]string, func()) {
	return CreateServerPodWithImage(t, kubeClient, namespace, name, labels, port, DefaultAgnhostImage)
}

// CreateServerPodWithImage is like CreateServerPod but allows specifying a
// custom agnhost image.
func CreateServerPodWithImage(t testing.TB, kubeClient kubernetes.Interface, namespace, name string, labels map[string]string, port int32, image string) ([]string, func()) {
	t.Helper()
	t.Logf("creating server pod %s/%s port=%d labels=%v", namespace, name, port, labels)

	pod := NetexecPodWithImage(name, namespace, labels, port, image)
	_, err := kubeClient.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create server pod %s/%s: %v", namespace, name, err)
	}

	if err := WaitForPodReady(kubeClient, namespace, name); err != nil {
		t.Fatalf("server pod %s/%s never became ready: %v", namespace, name, err)
	}

	created, err := kubeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get server pod %s/%s: %v", namespace, name, err)
	}

	ips := PodIPs(created)
	if len(ips) == 0 {
		t.Fatalf("server pod %s/%s has no IPs", namespace, name)
	}
	t.Logf("server pod %s/%s ips=%v", namespace, name, ips)

	return ips, func() {
		t.Logf("deleting server pod %s/%s", namespace, name)
		_ = kubeClient.CoreV1().Pods(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	}
}

// ----- Connectivity checks -----

// RunConnectivityCheck creates an ephemeral agnhost connect pod in the given
// namespace with the specified labels, attempts a TCP connection to
// serverIP:port, and returns whether the connection succeeded.
func RunConnectivityCheck(kubeClient kubernetes.Interface, namespace string, labels map[string]string, serverIP string, port int32) (bool, error) {
	return RunConnectivityCheckWithImage(kubeClient, namespace, labels, serverIP, port, DefaultAgnhostImage)
}

// RunConnectivityCheckWithImage is like RunConnectivityCheck but allows
// specifying a custom agnhost image.
func RunConnectivityCheckWithImage(kubeClient kubernetes.Interface, namespace string, labels map[string]string, serverIP string, port int32, image string) (bool, error) {
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
					Image: image,
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
						FormatIPPort(serverIP, port),
					},
				},
			},
		},
	}

	_, err := kubeClient.CoreV1().Pods(namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	defer func() {
		_ = kubeClient.CoreV1().Pods(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
	}()

	if err := WaitForPodCompletion(kubeClient, namespace, name); err != nil {
		return false, err
	}
	completed, err := kubeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if len(completed.Status.ContainerStatuses) == 0 {
		return false, fmt.Errorf("no container status recorded for pod %s", name)
	}
	exitCode := completed.Status.ContainerStatuses[0].State.Terminated.ExitCode
	return exitCode == 0, nil
}

// ExpectConnectivity checks connectivity from a pod in the given namespace
// (with clientLabels) to each serverIP on the specified port. The check is
// retried for up to 2 minutes per IP. If the result does not match
// shouldSucceed the test is failed via t.Fatalf.
func ExpectConnectivity(t testing.TB, kubeClient kubernetes.Interface, namespace string, clientLabels map[string]string, serverIPs []string, port int32, shouldSucceed bool) {
	t.Helper()
	for _, ip := range serverIPs {
		family := "IPv4"
		if IsIPv6(ip) {
			family = "IPv6"
		}
		t.Logf("checking %s connectivity %s -> %s expected=%t", family, namespace, FormatIPPort(ip, port), shouldSucceed)
		if err := pollConnectivity(kubeClient, namespace, clientLabels, ip, port, shouldSucceed, 2*time.Minute); err != nil {
			t.Fatalf("connectivity check failed for %s %s -> %s (expected %t): %v", family, namespace, FormatIPPort(ip, port), shouldSucceed, err)
		}
	}
}

func pollConnectivity(kubeClient kubernetes.Interface, namespace string, clientLabels map[string]string, serverIP string, port int32, shouldSucceed bool, timeout time.Duration) error {
	return wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		succeeded, err := RunConnectivityCheck(kubeClient, namespace, clientLabels, serverIP, port)
		if err != nil {
			return false, err
		}
		return succeeded == shouldSucceed, nil
	})
}

// LogConnectivityBestEffort is like ExpectConnectivity but uses a shorter
// timeout (30s) and only logs failures instead of failing the test. This is
// useful when external factors (e.g. other namespaces' egress policies, mTLS)
// can interfere with the check.
func LogConnectivityBestEffort(t testing.TB, kubeClient kubernetes.Interface, namespace string, clientLabels map[string]string, serverIPs []string, port int32, shouldSucceed bool) {
	t.Helper()
	for _, ip := range serverIPs {
		family := "IPv4"
		if IsIPv6(ip) {
			family = "IPv6"
		}
		t.Logf("checking %s connectivity (best-effort) %s -> %s expected=%t", family, namespace, FormatIPPort(ip, port), shouldSucceed)
		if err := pollConnectivity(kubeClient, namespace, clientLabels, ip, port, shouldSucceed, 30*time.Second); err != nil {
			t.Logf("connectivity (best-effort) %s -> %s expected=%t FAILED: %v", namespace, FormatIPPort(ip, port), shouldSucceed, err)
		}
	}
}

// ----- NetworkPolicy construction helpers -----

// DefaultDenyPolicy returns a NetworkPolicy that blocks all ingress and egress
// for every pod in the given namespace.
func DefaultDenyPolicy(name, namespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

// AllowIngressPolicy returns a NetworkPolicy that allows ingress to pods with
// podLabels from pods with fromLabels on the specified port.
func AllowIngressPolicy(name, namespace string, podLabels, fromLabels map[string]string, port int32) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podLabels},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{PodSelector: &metav1.LabelSelector{MatchLabels: fromLabels}},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Port: &intstr.IntOrString{Type: intstr.Int, IntVal: port}, Protocol: ProtocolPtr(corev1.ProtocolTCP)},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
}

// AllowEgressPolicy returns a NetworkPolicy that allows egress from pods with
// podLabels to pods with toLabels on the specified port.
func AllowEgressPolicy(name, namespace string, podLabels, toLabels map[string]string, port int32) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podLabels},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{
						{PodSelector: &metav1.LabelSelector{MatchLabels: toLabels}},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{Port: &intstr.IntOrString{Type: intstr.Int, IntVal: port}, Protocol: ProtocolPtr(corev1.ProtocolTCP)},
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
}

// ----- Policy inspection helpers (pure functions, no test framework) -----

// HasPort returns true if the given list of NetworkPolicy ports includes a port
// matching the specified protocol and port number.
func HasPort(ports []networkingv1.NetworkPolicyPort, protocol corev1.Protocol, port int32) bool {
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

// HasPortInIngress returns true if any ingress rule contains the specified
// protocol/port.
func HasPortInIngress(rules []networkingv1.NetworkPolicyIngressRule, protocol corev1.Protocol, port int32) bool {
	for _, rule := range rules {
		if HasPort(rule.Ports, protocol, port) {
			return true
		}
	}
	return false
}

// HasPortInEgress returns true if any egress rule contains the specified
// protocol/port.
func HasPortInEgress(rules []networkingv1.NetworkPolicyEgressRule, protocol corev1.Protocol, port int32) bool {
	for _, rule := range rules {
		if HasPort(rule.Ports, protocol, port) {
			return true
		}
	}
	return false
}

// HasDefaultDeny returns true if any policy in the list is a default-deny-all
// (empty podSelector with both Ingress and Egress policyTypes).
func HasDefaultDeny(policies []networkingv1.NetworkPolicy) bool {
	for _, policy := range policies {
		if len(policy.Spec.PodSelector.MatchLabels) != 0 || len(policy.Spec.PodSelector.MatchExpressions) != 0 {
			continue
		}
		if HasPolicyTypes(policy.Spec.PolicyTypes, networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress) {
			return true
		}
	}
	return false
}

// HasPolicyTypes returns true if the given policyTypes list contains all of the
// expected types.
func HasPolicyTypes(policyTypes []networkingv1.PolicyType, expected ...networkingv1.PolicyType) bool {
	expectedSet := map[networkingv1.PolicyType]struct{}{}
	for _, typ := range expected {
		expectedSet[typ] = struct{}{}
	}
	for _, typ := range policyTypes {
		delete(expectedSet, typ)
	}
	return len(expectedSet) == 0
}

// HasEgressPortInNamespace returns true if any policy in the list has an egress
// rule with the specified protocol/port.
func HasEgressPortInNamespace(policies []networkingv1.NetworkPolicy, protocol corev1.Protocol, port int32) bool {
	for _, policy := range policies {
		if HasPortInEgress(policy.Spec.Egress, protocol, port) {
			return true
		}
	}
	return false
}

// HasUnrestrictedEgressInNamespace returns true if any policy in the list has
// an egress rule with no port and no destination restrictions (i.e. allows all
// egress).
func HasUnrestrictedEgressInNamespace(policies []networkingv1.NetworkPolicy) bool {
	for _, policy := range policies {
		for _, rule := range policy.Spec.Egress {
			if len(rule.Ports) == 0 && len(rule.To) == 0 {
				return true
			}
		}
	}
	return false
}

// HasIngressFromNamespace returns true if any ingress rule allows traffic from
// the specified namespace on the given port (TCP).
func HasIngressFromNamespace(rules []networkingv1.NetworkPolicyIngressRule, port int32, namespace string) bool {
	for _, rule := range rules {
		if !HasPort(rule.Ports, corev1.ProtocolTCP, port) {
			continue
		}
		for _, peer := range rule.From {
			if NamespaceSelectorMatchesNamespace(peer.NamespaceSelector, namespace) {
				return true
			}
		}
	}
	return false
}

// HasIngressAllowAll returns true if any ingress rule allows traffic from all
// sources on the specified port.
func HasIngressAllowAll(rules []networkingv1.NetworkPolicyIngressRule, port int32) bool {
	for _, rule := range rules {
		if !HasPort(rule.Ports, corev1.ProtocolTCP, port) {
			continue
		}
		if len(rule.From) == 0 {
			return true
		}
	}
	return false
}

// HasIngressFromPolicyGroup returns true if any ingress rule allows traffic
// from namespaces with the given policy-group label key on the specified port.
func HasIngressFromPolicyGroup(rules []networkingv1.NetworkPolicyIngressRule, port int32, policyGroupLabelKey string) bool {
	for _, rule := range rules {
		if !HasPort(rule.Ports, corev1.ProtocolTCP, port) {
			continue
		}
		for _, peer := range rule.From {
			if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels == nil {
				continue
			}
			if _, ok := peer.NamespaceSelector.MatchLabels[policyGroupLabelKey]; ok {
				return true
			}
		}
	}
	return false
}

// HasEgressAllowAllTCP returns true if any egress rule allows all TCP traffic
// (no destination restriction).
func HasEgressAllowAllTCP(rules []networkingv1.NetworkPolicyEgressRule) bool {
	for _, rule := range rules {
		if len(rule.To) != 0 {
			continue
		}
		if HasAnyTCPPort(rule.Ports) {
			return true
		}
	}
	return false
}

// HasAnyTCPPort returns true if the ports list is empty (all ports) or contains
// at least one TCP port.
func HasAnyTCPPort(ports []networkingv1.NetworkPolicyPort) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if p.Protocol != nil && *p.Protocol != corev1.ProtocolTCP {
			continue
		}
		return true
	}
	return false
}

// NamespaceSelectorMatchesNamespace returns true if the given label selector
// matches the namespace by checking the "kubernetes.io/metadata.name" label in
// both MatchLabels and MatchExpressions. Returns false when the selector is
// nil (meaning no namespace selector was specified on the peer).
func NamespaceSelectorMatchesNamespace(selector *metav1.LabelSelector, namespace string) bool {
	if selector == nil {
		return false
	}
	if selector.MatchLabels != nil {
		if selector.MatchLabels["kubernetes.io/metadata.name"] == namespace {
			return true
		}
	}
	for _, expr := range selector.MatchExpressions {
		if expr.Key != "kubernetes.io/metadata.name" {
			continue
		}
		if expr.Operator != metav1.LabelSelectorOpIn {
			continue
		}
		for _, value := range expr.Values {
			if value == namespace {
				return true
			}
		}
	}
	return false
}

// ----- Policy evaluation helpers (used in enforcement tests) -----

// IngressAllowsFromNamespace returns true if the given NetworkPolicy's ingress
// rules allow traffic from the specified namespace with the given pod labels on
// the specified port.
func IngressAllowsFromNamespace(policy *networkingv1.NetworkPolicy, namespace string, labels map[string]string, port int32) bool {
	for _, rule := range policy.Spec.Ingress {
		if !RuleAllowsPort(rule.Ports, port) {
			continue
		}
		if len(rule.From) == 0 {
			return true
		}
		for _, peer := range rule.From {
			if peer.NamespaceSelector != nil {
				if NamespaceSelectorMatchesNamespace(peer.NamespaceSelector, namespace) && PodMatch(peer.PodSelector, labels) {
					return true
				}
				continue
			}
			if PodMatch(peer.PodSelector, labels) {
				return true
			}
		}
	}
	return false
}

// EgressAllowsNamespace returns true if the given NetworkPolicy's egress rules
// allow traffic to the specified namespace on the specified port.
func EgressAllowsNamespace(policy *networkingv1.NetworkPolicy, namespace string, port int32) bool {
	for _, rule := range policy.Spec.Egress {
		if !RuleAllowsPort(rule.Ports, port) {
			continue
		}
		if len(rule.To) == 0 {
			return true
		}
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil && NamespaceSelectorMatchesNamespace(peer.NamespaceSelector, namespace) {
				return true
			}
		}
	}
	return false
}

// PodMatch returns true if the given label selector matches the provided
// labels.
func PodMatch(selector *metav1.LabelSelector, labels map[string]string) bool {
	if selector == nil {
		return true
	}
	for key, value := range selector.MatchLabels {
		if labels[key] != value {
			return false
		}
	}
	return true
}

// RuleAllowsPort returns true if the given list of policy ports includes the
// specified port (or is empty, meaning all ports are allowed).
func RuleAllowsPort(ports []networkingv1.NetworkPolicyPort, port int32) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if p.Port == nil {
			continue
		}
		if p.Port.Type == intstr.Int && p.Port.IntVal == port {
			return true
		}
	}
	return false
}

// ----- Policy assertion helpers (use testing.TB) -----

// GetNetworkPolicy fetches a NetworkPolicy by namespace and name, failing the
// test if it does not exist.
func GetNetworkPolicy(t testing.TB, ctx context.Context, client kubernetes.Interface, namespace, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	policy, err := client.NetworkingV1().NetworkPolicies(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get NetworkPolicy %s/%s: %v", namespace, name, err)
	}
	return policy
}

// RequirePodSelectorLabel asserts that the policy's podSelector contains the
// given key=value label.
func RequirePodSelectorLabel(t testing.TB, policy *networkingv1.NetworkPolicy, key, value string) {
	t.Helper()
	actual, ok := policy.Spec.PodSelector.MatchLabels[key]
	if !ok || actual != value {
		t.Fatalf("%s/%s: expected podSelector %s=%s, got %v", policy.Namespace, policy.Name, key, value, policy.Spec.PodSelector.MatchLabels)
	}
}

// RequireEmptyPodSelector asserts that the policy's podSelector is empty
// (selects all pods in the namespace).
func RequireEmptyPodSelector(t testing.TB, policy *networkingv1.NetworkPolicy) {
	t.Helper()
	if len(policy.Spec.PodSelector.MatchLabels) != 0 || len(policy.Spec.PodSelector.MatchExpressions) != 0 {
		t.Fatalf("%s/%s: expected empty podSelector, got matchLabels=%v matchExpressions=%v",
			policy.Namespace, policy.Name, policy.Spec.PodSelector.MatchLabels, policy.Spec.PodSelector.MatchExpressions)
	}
}

// RequireDefaultDenyAll asserts that the policy is a default-deny-all: empty
// podSelector with both Ingress and Egress policyTypes.
func RequireDefaultDenyAll(t testing.TB, policy *networkingv1.NetworkPolicy) {
	t.Helper()
	if len(policy.Spec.PodSelector.MatchLabels) != 0 || len(policy.Spec.PodSelector.MatchExpressions) != 0 {
		t.Fatalf("%s/%s: expected empty podSelector for default-deny, got matchLabels=%v matchExpressions=%v",
			policy.Namespace, policy.Name, policy.Spec.PodSelector.MatchLabels, policy.Spec.PodSelector.MatchExpressions)
	}
	if !HasPolicyTypes(policy.Spec.PolicyTypes, networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress) {
		t.Fatalf("%s/%s: expected both Ingress and Egress policyTypes, got %v", policy.Namespace, policy.Name, policy.Spec.PolicyTypes)
	}
}

// RequireIngressPort asserts that the policy has an ingress rule with the
// specified protocol and port.
func RequireIngressPort(t testing.TB, policy *networkingv1.NetworkPolicy, protocol corev1.Protocol, port int32) {
	t.Helper()
	if !HasPortInIngress(policy.Spec.Ingress, protocol, port) {
		t.Fatalf("%s/%s: expected ingress port %s/%d", policy.Namespace, policy.Name, protocol, port)
	}
}

// RequireEgressPort asserts that the policy has an egress rule with the
// specified protocol and port.
func RequireEgressPort(t testing.TB, policy *networkingv1.NetworkPolicy, protocol corev1.Protocol, port int32) {
	t.Helper()
	if !HasPortInEgress(policy.Spec.Egress, protocol, port) {
		t.Fatalf("%s/%s: expected egress port %s/%d", policy.Namespace, policy.Name, protocol, port)
	}
}

// RequireUnrestrictedEgress asserts that the policy has exactly one egress rule
// with no port and no destination restrictions (allows all egress).
func RequireUnrestrictedEgress(t testing.TB, policy *networkingv1.NetworkPolicy) {
	t.Helper()
	if len(policy.Spec.Egress) != 1 {
		t.Fatalf("%s/%s: expected exactly one egress rule for unrestricted egress, got %d rules",
			policy.Namespace, policy.Name, len(policy.Spec.Egress))
	}
	egressRule := policy.Spec.Egress[0]
	if len(egressRule.Ports) != 0 || len(egressRule.To) != 0 {
		t.Fatalf("%s/%s: expected unrestricted egress rule (no ports, no to), got ports=%v to=%v",
			policy.Namespace, policy.Name, egressRule.Ports, egressRule.To)
	}
}

// RequireIngressFromNamespace asserts that the policy allows ingress from the
// specified namespace on the given port.
func RequireIngressFromNamespace(t testing.TB, policy *networkingv1.NetworkPolicy, port int32, namespace string) {
	t.Helper()
	if !HasIngressFromNamespace(policy.Spec.Ingress, port, namespace) {
		t.Fatalf("%s/%s: expected ingress from namespace %s on port %d", policy.Namespace, policy.Name, namespace, port)
	}
}

// RequireIngressFromNamespaceOrPolicyGroup asserts that the policy allows
// ingress either from the specified namespace or from namespaces with the given
// policy-group label on the specified port.
func RequireIngressFromNamespaceOrPolicyGroup(t testing.TB, policy *networkingv1.NetworkPolicy, port int32, namespace, policyGroupLabelKey string) {
	t.Helper()
	if HasIngressFromNamespace(policy.Spec.Ingress, port, namespace) {
		return
	}
	if HasIngressFromPolicyGroup(policy.Spec.Ingress, port, policyGroupLabelKey) {
		return
	}
	t.Fatalf("%s/%s: expected ingress from namespace %s or policy-group %s on port %d", policy.Namespace, policy.Name, namespace, policyGroupLabelKey, port)
}

// RequireIngressAllowAll asserts that the policy allows ingress from any source
// on the specified port.
func RequireIngressAllowAll(t testing.TB, policy *networkingv1.NetworkPolicy, port int32) {
	t.Helper()
	if !HasIngressAllowAll(policy.Spec.Ingress, port) {
		t.Fatalf("%s/%s: expected ingress allow-all on port %d", policy.Namespace, policy.Name, port)
	}
}

// ----- Policy reconciliation helpers -----

// RestoreNetworkPolicy deletes the given network policy and waits for the
// operator to recreate it with the expected spec. The timeout controls how long
// to wait for restoration.
func RestoreNetworkPolicy(t testing.TB, ctx context.Context, client kubernetes.Interface, expected *networkingv1.NetworkPolicy, timeout time.Duration) {
	t.Helper()
	namespace := expected.Namespace
	name := expected.Name
	t.Logf("deleting NetworkPolicy %s/%s and waiting for restoration", namespace, name)
	if err := client.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("failed to delete NetworkPolicy %s/%s: %v", namespace, name, err)
	}
	err := wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		current, err := client.NetworkingV1().NetworkPolicies(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return equality.Semantic.DeepEqual(expected.Spec, current.Spec), nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for NetworkPolicy %s/%s spec to be restored", namespace, name)
	}
	t.Logf("NetworkPolicy %s/%s spec restored after delete", namespace, name)
}

// MutateAndRestoreNetworkPolicy patches the policy's podSelector with a
// spurious label, then waits for the operator to reconcile it back to the
// original spec. The timeout controls how long to wait for reconciliation.
func MutateAndRestoreNetworkPolicy(t testing.TB, ctx context.Context, client kubernetes.Interface, namespace, name string, timeout time.Duration) {
	t.Helper()
	original := GetNetworkPolicy(t, ctx, client, namespace, name)
	t.Logf("mutating NetworkPolicy %s/%s (podSelector override) and waiting for reconciliation", namespace, name)
	patch := []byte(`{"spec":{"podSelector":{"matchLabels":{"np-reconcile":"mutated"}}}}`)
	_, err := client.NetworkingV1().NetworkPolicies(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("failed to patch NetworkPolicy %s/%s: %v", namespace, name, err)
	}

	err = wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		current := GetNetworkPolicy(t, ctx, client, namespace, name)
		return equality.Semantic.DeepEqual(original.Spec, current.Spec), nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for NetworkPolicy %s/%s spec to be restored after mutation", namespace, name)
	}
	t.Logf("NetworkPolicy %s/%s spec restored after mutation", namespace, name)
}

// ----- Logging helpers -----

// LogPolicyNames logs the names of all NetworkPolicies in the given list.
func LogPolicyNames(t testing.TB, namespace string, policies []networkingv1.NetworkPolicy) {
	t.Helper()
	names := make([]string, 0, len(policies))
	for _, policy := range policies {
		names = append(names, policy.Name)
	}
	t.Logf("networkpolicies in %s: %v", namespace, names)
}

// LogNetworkPolicySummary logs a one-line summary of a NetworkPolicy.
func LogNetworkPolicySummary(t testing.TB, label string, policy *networkingv1.NetworkPolicy) {
	t.Helper()
	t.Logf("networkpolicy %s namespace=%s name=%s podSelector=%v policyTypes=%v ingress=%d egress=%d",
		label,
		policy.Namespace,
		policy.Name,
		policy.Spec.PodSelector.MatchLabels,
		policy.Spec.PolicyTypes,
		len(policy.Spec.Ingress),
		len(policy.Spec.Egress),
	)
}

// LogNetworkPolicyDetails logs detailed ingress and egress rules.
func LogNetworkPolicyDetails(t testing.TB, label string, policy *networkingv1.NetworkPolicy) {
	t.Helper()
	t.Logf("networkpolicy %s details:", label)
	t.Logf("  podSelector=%v policyTypes=%v", policy.Spec.PodSelector.MatchLabels, policy.Spec.PolicyTypes)
	for i, rule := range policy.Spec.Ingress {
		t.Logf("  ingress[%d]: ports=%s from=%s", i, FormatPorts(rule.Ports), FormatPeers(rule.From))
	}
	for i, rule := range policy.Spec.Egress {
		t.Logf("  egress[%d]: ports=%s to=%s", i, FormatPorts(rule.Ports), FormatPeers(rule.To))
	}
}

// LogIngressFromNamespaceOptional logs whether ingress from the specified
// namespace is present on the given port (informational, does not fail).
func LogIngressFromNamespaceOptional(t testing.TB, policy *networkingv1.NetworkPolicy, port int32, namespace string) {
	t.Helper()
	if HasIngressFromNamespace(policy.Spec.Ingress, port, namespace) {
		t.Logf("networkpolicy %s/%s: ingress from namespace %s present on port %d", policy.Namespace, policy.Name, namespace, port)
		return
	}
	t.Logf("networkpolicy %s/%s: no ingress from namespace %s on port %d", policy.Namespace, policy.Name, namespace, port)
}

// LogIngressHostNetworkOrAllowAll logs whether the policy has an allow-all
// ingress rule or a host-network policy-group rule on the given port.
func LogIngressHostNetworkOrAllowAll(t testing.TB, policy *networkingv1.NetworkPolicy, port int32) {
	t.Helper()
	if HasIngressAllowAll(policy.Spec.Ingress, port) {
		t.Logf("networkpolicy %s/%s: ingress allow-all present on port %d", policy.Namespace, policy.Name, port)
		return
	}
	if HasIngressFromPolicyGroup(policy.Spec.Ingress, port, "policy-group.network.openshift.io/host-network") {
		t.Logf("networkpolicy %s/%s: ingress host-network policy-group present on port %d", policy.Namespace, policy.Name, port)
		return
	}
	t.Logf("networkpolicy %s/%s: no ingress allow-all/host-network rule on port %d", policy.Namespace, policy.Name, port)
}

// LogEgressAllowAllTCP logs whether the policy has an egress allow-all TCP
// rule.
func LogEgressAllowAllTCP(t testing.TB, policy *networkingv1.NetworkPolicy) {
	t.Helper()
	if HasEgressAllowAllTCP(policy.Spec.Egress) {
		t.Logf("networkpolicy %s/%s: egress allow-all TCP rule present", policy.Namespace, policy.Name)
		return
	}
	t.Logf("networkpolicy %s/%s: no egress allow-all TCP rule", policy.Namespace, policy.Name)
}

// LogNetworkPolicyEvents searches for NetworkPolicy-related events in the
// given namespaces (best-effort, does not fail).
//
// Events emitted by the resourceapply package in library-go use the operator
// Deployment as the InvolvedObject (not the NetworkPolicy itself).  The event
// Reason is prefixed with "NetworkPolicy" (e.g. NetworkPolicyCreated,
// NetworkPolicyUpdated, NetworkPolicyDeleted) and the event Message contains
// the full resource reference including the policy name.  Therefore this
// function matches events by:
//   - Reason starting with "NetworkPolicy", OR
//   - Message containing the policyName, OR
//   - InvolvedObject.Kind == "NetworkPolicy" (for any recorder that does
//     reference the policy directly).
//
// Callers should include the **operator** namespace in the namespaces list
// because that is where resourceapply records the events.
func LogNetworkPolicyEvents(t testing.TB, ctx context.Context, client kubernetes.Interface, namespaces []string, policyName string) {
	t.Helper()
	found := false
	_ = wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		for _, namespace := range namespaces {
			eventList, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				t.Logf("unable to list events in %s: %v", namespace, err)
				continue
			}
			for _, event := range eventList.Items {
				isNPEvent := false

				// Match by Reason prefix (e.g. NetworkPolicyCreated/Updated/Deleted)
				if strings.HasPrefix(event.Reason, "NetworkPolicy") {
					isNPEvent = true
				}

				// Match by InvolvedObject being a NetworkPolicy directly
				if event.InvolvedObject.Kind == "NetworkPolicy" {
					isNPEvent = true
				}

				// Match by Message containing the policy name
				if policyName != "" && strings.Contains(event.Message, policyName) {
					isNPEvent = true
				}

				if isNPEvent {
					t.Logf("event in %s: type=%s reason=%s involvedObject=%s/%s message=%q",
						namespace, event.Type, event.Reason,
						event.InvolvedObject.Kind, event.InvolvedObject.Name,
						event.Message)
					found = true
				}
			}
		}
		if found {
			return true, nil
		}
		t.Logf("no NetworkPolicy events yet for %s (namespaces: %v)", policyName, namespaces)
		return false, nil
	})
	if !found {
		t.Logf("no NetworkPolicy events observed for %s (best-effort)", policyName)
	}
}

// ----- Format helpers -----

// FormatPorts returns a human-readable string of a port list.
func FormatPorts(ports []networkingv1.NetworkPolicyPort) string {
	if len(ports) == 0 {
		return "[]"
	}
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		proto := "TCP"
		if p.Protocol != nil {
			proto = string(*p.Protocol)
		}
		if p.Port == nil {
			out = append(out, fmt.Sprintf("%s:any", proto))
			continue
		}
		out = append(out, fmt.Sprintf("%s:%s", proto, p.Port.String()))
	}
	return fmt.Sprintf("[%s]", strings.Join(out, ", "))
}

// FormatPeers returns a human-readable string of a peer list.
func FormatPeers(peers []networkingv1.NetworkPolicyPeer) string {
	if len(peers) == 0 {
		return "[]"
	}
	out := make([]string, 0, len(peers))
	for _, peer := range peers {
		ns := FormatSelector(peer.NamespaceSelector)
		pod := FormatSelector(peer.PodSelector)
		if ns == "" && pod == "" {
			out = append(out, "{}")
			continue
		}
		out = append(out, fmt.Sprintf("ns=%s pod=%s", ns, pod))
	}
	return fmt.Sprintf("[%s]", strings.Join(out, ", "))
}

// FormatSelector returns a human-readable string of a label selector.
func FormatSelector(sel *metav1.LabelSelector) string {
	if sel == nil {
		return ""
	}
	if len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0 {
		return "{}"
	}
	return fmt.Sprintf("labels=%v exprs=%v", sel.MatchLabels, sel.MatchExpressions)
}

// ----- Wait helpers -----

// WaitForPodReady waits up to 2 minutes for a pod to reach the Running phase
// with a Ready condition.
func WaitForPodReady(kubeClient kubernetes.Interface, namespace, name string) error {
	return wait.PollImmediate(2*time.Second, 2*time.Minute, func() (bool, error) {
		pod, err := kubeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if pod.Status.Phase != corev1.PodRunning {
			return false, nil
		}
		return IsPodReady(pod), nil
	})
}

// WaitForPodCompletion waits up to 2 minutes for a pod to reach Succeeded or
// Failed phase.
func WaitForPodCompletion(kubeClient kubernetes.Interface, namespace, name string) error {
	return wait.PollImmediate(2*time.Second, 2*time.Minute, func() (bool, error) {
		pod, err := kubeClient.CoreV1().Pods(namespace).Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed, nil
	})
}

// WaitForPodsReadyByLabel waits up to 5 minutes for all pods matching the
// label selector in the namespace to be ready.
func WaitForPodsReadyByLabel(t testing.TB, ctx context.Context, client kubernetes.Interface, namespace, labelSelector string) {
	t.Helper()
	t.Logf("waiting for pods ready in %s with selector %s", namespace, labelSelector)
	err := wait.PollImmediate(5*time.Second, 5*time.Minute, func() (bool, error) {
		pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}
		if len(pods.Items) == 0 {
			return false, nil
		}
		for _, pod := range pods.Items {
			if !IsPodReady(&pod) {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for pods in %s with selector %s to be ready", namespace, labelSelector)
	}
}

// IsPodReady returns true if the pod has a Ready condition set to True.
func IsPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// ----- Utility helpers -----

// BoolToAllowDeny returns "allow" if allow is true, "deny" otherwise.
func BoolToAllowDeny(allow bool) string {
	if allow {
		return "allow"
	}
	return "deny"
}

// ProtocolPtr returns a pointer to the given Protocol.
func ProtocolPtr(protocol corev1.Protocol) *corev1.Protocol {
	return &protocol
}

func boolptr(value bool) *bool {
	return &value
}

func int64ptr(value int64) *int64 {
	return &value
}
