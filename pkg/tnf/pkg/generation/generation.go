package generation

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
)

const (
	Namespace       = "openshift-etcd"
	Label           = "tnf.etcd.openshift.io/generation"
	ComponentLabel  = "app.kubernetes.io/component"
	Component       = "tnf-update-setup"
	ConfigMapEnv    = "TNF_UPDATE_SETUP_CONFIGMAP"
	ConfigMapUIDEnv = "TNF_UPDATE_SETUP_CONFIGMAP_UID"
	CompletedJob    = "tnf.etcd.openshift.io/completed-job"
	CompletedAt     = "tnf.etcd.openshift.io/completed-at"
	DataKey         = "config.json"
)

// Node records identity independently of readiness or Pacemaker's online state.
type Node struct {
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
	IP   string    `json:"ip"`
}

// Snapshot is immutable. Completion is recorded separately on the ConfigMap metadata.
type Snapshot struct {
	Repair     bool   `json:"repair,omitempty"`
	Version    int    `json:"version"`
	Generation int64  `json:"generation"`
	Nodes      []Node `json:"nodes"`
	Previous   []Node `json:"previous,omitempty"`
	Image      string `json:"image"`
}

func Nodes(nodes []*corev1.Node) ([]Node, error) {
	result := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		ip, err := tools.GetNodeIPForPacemaker(*node)
		if err != nil {
			return nil, err
		}
		result = append(result, Node{Name: node.Name, UID: node.UID, IP: ip})
	}
	if err := validateNodes(result); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	for i := range result {
		result[i].IP = net.ParseIP(result[i].IP).String()
	}
	return result, nil
}

func validateNodes(nodes []Node) error {
	if len(nodes) < 1 || len(nodes) > 2 {
		return fmt.Errorf("expected one or two control-plane nodes, got %d", len(nodes))
	}
	names, ips, uids := map[string]bool{}, map[string]bool{}, map[types.UID]bool{}
	for _, n := range nodes {
		ip := net.ParseIP(n.IP)
		if len(validation.IsDNS1123Subdomain(n.Name)) != 0 || n.UID == "" || len(validation.IsValidLabelValue(string(n.UID))) != 0 || ip == nil {
			return fmt.Errorf("invalid node identity: name=%q uid=%q ip=%q", n.Name, n.UID, n.IP)
		}
		if names[n.Name] || ips[ip.String()] || uids[n.UID] {
			return fmt.Errorf("duplicate node identity for %s", n.Name)
		}
		names[n.Name], ips[ip.String()], uids[n.UID] = true, true, true
	}
	return nil
}

func Name(number int64) string                 { return fmt.Sprintf("tnf-update-setup-%d", number) }
func JobName(number int64, attempt int) string { return fmt.Sprintf("%s-%d", Name(number), attempt) }
func SameNodes(a, b []Node) bool               { return reflect.DeepEqual(a, b) }

func (s Snapshot) Validate() error {
	if s.Version != 1 || s.Generation < 1 {
		return fmt.Errorf("unsupported snapshot version/generation: %d/%d", s.Version, s.Generation)
	}
	if err := validateNodes(s.Nodes); err != nil {
		return err
	}
	if len(s.Previous) > 0 {
		return validateNodes(s.Previous)
	}
	return nil
}

func (s Snapshot) ConfigMap() (*corev1.ConfigMap, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: Name(s.Generation), Namespace: Namespace, Labels: map[string]string{ComponentLabel: Component, Label: strconv.FormatInt(s.Generation, 10)}}, Immutable: ptr.To(true), Data: map[string]string{DataKey: string(data)}}, nil
}

func Decode(cm *corev1.ConfigMap) (Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal([]byte(cm.Data[DataKey]), &s); err != nil {
		return s, err
	}
	if err := s.Validate(); err != nil {
		return s, err
	}
	if cm.Name != Name(s.Generation) || cm.Labels[Label] != strconv.FormatInt(s.Generation, 10) || cm.Labels[ComponentLabel] != Component || !ptr.Deref(cm.Immutable, false) {
		return s, fmt.Errorf("snapshot identity does not match ConfigMap %s", cm.Name)
	}
	return s, nil
}

func Records(ctx context.Context, client kubernetes.Interface) ([]corev1.ConfigMap, error) {
	list, err := client.CoreV1().ConfigMaps(Namespace).List(ctx, metav1.ListOptions{LabelSelector: ComponentLabel + "=" + Component})
	if err != nil {
		return nil, err
	}
	for i := range list.Items {
		if _, err := Decode(&list.Items[i]); err != nil {
			return nil, fmt.Errorf("invalid generation record %s: %w", list.Items[i].Name, err)
		}
	}
	sort.Slice(list.Items, func(i, j int) bool {
		a, _ := Decode(&list.Items[i])
		b, _ := Decode(&list.Items[j])
		return a.Generation < b.Generation
	})
	return list.Items, nil
}

// MatchesCurrent refuses a stale snapshot, including same-name replacements.
func (s Snapshot) MatchesCurrent(ctx context.Context, client kubernetes.Interface) error {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: tools.ControlPlaneNodeLabelSelector})
	if err != nil {
		return err
	}
	items := make([]*corev1.Node, 0, len(nodes.Items))
	for i := range nodes.Items {
		items = append(items, &nodes.Items[i])
	}
	current, err := Nodes(items)
	if err != nil {
		return err
	}
	if !SameNodes(s.Nodes, current) {
		return fmt.Errorf("generation %d no longer matches Kubernetes node identities", s.Generation)
	}
	return nil
}

// MembershipChanges never interprets an offline registered member as deleted.
func (s Snapshot) MembershipChanges(configured map[string]string) (remove, add []string) {
	desired := map[string]Node{}
	for _, n := range s.Nodes {
		desired[n.Name] = n
	}
	for name, ip := range configured {
		n, ok := desired[name]
		if !ok || !net.ParseIP(ip).Equal(net.ParseIP(n.IP)) {
			remove = append(remove, name)
		}
	}
	for _, n := range s.Nodes {
		ip, ok := configured[n.Name]
		if !ok || !net.ParseIP(ip).Equal(net.ParseIP(n.IP)) {
			add = append(add, n.Name)
		}
	}
	sort.Strings(remove)
	sort.Strings(add)
	return
}

// DeferDuringUpgrade permits only real membership repair while MCO/CVO progresses.
func (s Snapshot) DeferDuringUpgrade(progressing bool, configured map[string]string) bool {
	remove, add := s.MembershipChanges(configured)
	if len(remove)+len(add) > 0 {
		return false
	}
	for _, old := range s.Previous {
		for _, n := range s.Nodes {
			if old.Name == n.Name && old.UID != n.UID {
				return false
			}
		}
	}
	return progressing
}
