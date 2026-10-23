package updatesetup

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/exec"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/generation"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
)

type recordedRunner struct {
	reconciled       bool // This generation previously completed membership reconciliation.
	continuingRepair bool
	repairClaim      string
	fencing          func(context.Context, kubernetes.Interface, []string) error
	alerts           func(context.Context) error
	waitForRevision  func(context.Context) error
	kube             kubernetes.Interface
	snapshot         generation.Snapshot
	current          string
	execute          func(context.Context, string) (string, string, error)
}

func runRecordedGeneration(ctx context.Context, kube kubernetes.Interface, current string, waitForRevision func(context.Context) error) error {
	name, uid := os.Getenv(generation.ConfigMapEnv), os.Getenv(generation.ConfigMapUIDEnv)
	if name == "" || uid == "" {
		return fmt.Errorf("update-setup requires an explicit recorded generation and ConfigMap UID")
	}
	cm, err := kube.CoreV1().ConfigMaps(generation.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(cm.UID) != uid {
		return fmt.Errorf("recorded generation ConfigMap UID changed")
	}
	snapshot, err := generation.Decode(cm)
	if err != nil {
		return err
	}
	r := recordedRunner{reconciled: cm.Annotations[generation.CompletedAt] != "", kube: kube, snapshot: snapshot, current: current, execute: exec.Execute, fencing: pcs.ConfigureFencing, alerts: pcs.ConfigureAlertsWithRetry, waitForRevision: waitForRevision}
	return r.run(ctx)
}

func (r *recordedRunner) configured(ctx context.Context) (map[string]string, error) {
	stdout, stderr, err := r.execute(ctx, "/usr/sbin/pcs cluster config show --output-format json")
	if err != nil {
		return nil, fmt.Errorf("reading configured membership: %w: %s", err, stderr)
	}
	var cfg pacemaker.ClusterConfig
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		return nil, err
	}
	if len(cfg.Nodes) == 0 {
		return nil, fmt.Errorf("refusing to reconcile empty Pacemaker configuration")
	}
	result := map[string]string{}
	for _, node := range cfg.Nodes {
		if len(node.Addrs) == 0 || net.ParseIP(node.Addrs[0].Addr) == nil || !validCommandNode(node.Name) {
			return nil, fmt.Errorf("incomplete configured member %q", node.Name)
		}
		if _, exists := result[node.Name]; exists {
			return nil, fmt.Errorf("duplicate configured member %q", node.Name)
		}
		result[node.Name] = node.Addrs[0].Addr
	}
	return result, nil
}

func (r *recordedRunner) guard(ctx context.Context) error {
	if err := r.snapshot.MatchesCurrent(ctx, r.kube); err != nil {
		return err
	}
	mcoUpdating, err := generation.MCOUpdateInProgress(ctx, r.kube)
	if err != nil {
		return err
	}
	configured, err := r.configured(ctx)
	if err != nil {
		return err
	}
	policySnapshot := r.snapshot
	if r.reconciled {
		remove, add := r.snapshot.MembershipChanges(configured)
		if len(remove)+len(add) > 0 {
			return fmt.Errorf("membership drift after completion requires a new recorded generation")
		}
		policySnapshot.Previous = nil
	}
	if policySnapshot.DeferDuringMCO(mcoUpdating, configured) && !r.continuingRepair {
		return fmt.Errorf("deferring generation %d during control-plane MCO activity", r.snapshot.Generation)
	}
	return nil
}

func (r *recordedRunner) command(ctx context.Context, command string) error {
	if err := r.guard(ctx); err != nil {
		return err
	}
	_, stderr, err := r.execute(ctx, command)
	if err != nil {
		return fmt.Errorf("%s: %w: %s", command, err, stderr)
	}
	return nil
}

func (r *recordedRunner) run(ctx context.Context) error {
	properties, err := r.properties(ctx)
	if err != nil {
		return err
	}
	r.continuingRepair = !r.reconciled && properties["tnf_reconciliation_generation"] == fmt.Sprint(r.snapshot.Generation)
	r.repairClaim = ""
	if r.continuingRepair {
		r.repairClaim = properties["tnf_reconciliation_force_new_cluster"]
		claim, err := strconv.ParseInt(r.repairClaim, 10, 64)
		if err != nil || claim <= 0 {
			return fmt.Errorf("repair generation %d has no valid recovery claim", r.snapshot.Generation)
		}
	}
	if err := r.guard(ctx); err != nil {
		return err
	}
	configured, err := r.configured(ctx)
	if err != nil {
		return err
	}
	if _, ok := configured[r.current]; !ok {
		return fmt.Errorf("execution node %s is not a configured Pacemaker member", r.current)
	}
	if _, _, err := r.execute(ctx, "/usr/sbin/pcs cluster status"); err != nil {
		return err
	}
	remove, add := r.snapshot.MembershipChanges(configured)
	// A changed UID is a replacement even if its name and IP were reused. A persistent
	// Pacemaker attribute makes a retried generation recognize an already repaired node.
	for _, n := range r.snapshot.Nodes {
		if _, ok := configured[n.Name]; !ok {
			continue
		}
		uid := properties["tnf_node_uid_"+n.Name]
		replaced := false
		if strings.TrimSpace(uid) != "" {
			replaced = strings.TrimSpace(uid) != string(n.UID)
		} else {
			for _, old := range r.snapshot.Previous {
				if old.Name == n.Name && old.UID != n.UID {
					replaced = true
				}
			}
		}
		if replaced && !containsName(remove, n.Name) {
			remove = append(remove, n.Name)
			if !containsName(add, n.Name) {
				add = append(add, n.Name)
			}
		}
	}
	sort.Strings(remove)
	sort.Strings(add)
	if r.snapshot.Discovery && len(remove)+len(add) == 0 && !r.continuingRepair {
		// The observation path may be unavailable while membership is correct.
		// In particular, do not restart an intentionally stopped registered node.
		return r.snapshot.MatchesCurrent(ctx, r.kube)
	}
	if len(remove)+len(add) > 0 {
		if r.reconciled {
			return fmt.Errorf("identity drift after completion requires a new recorded generation")
		}
		// Match main: replacements wait for manifests before removing a member;
		// adding back an already-missing member must remain able to recover.
		if len(remove) > 0 && len(add) > 0 {
			if err := r.waitForRevision(ctx); err != nil {
				return fmt.Errorf("waiting for stable etcd revision before replacement: %w", err)
			}
		}
		if !r.continuingRepair {
			// Persist one timestamp before changing membership. Retrying a partial
			// repair must not issue a new force_new_cluster claim each time.
			r.repairClaim = strconv.FormatInt(time.Now().Unix(), 10)
			if err := r.command(ctx, "crm_attribute --type crm_config --name tnf_reconciliation_force_new_cluster --update "+r.repairClaim); err != nil {
				return err
			}
			if err := r.command(ctx, fmt.Sprintf("crm_attribute --type crm_config --name tnf_reconciliation_generation --update %d", r.snapshot.Generation)); err != nil {
				return err
			}
		}
		r.continuingRepair = true
	}

	for _, name := range remove {
		if name == r.current {
			return fmt.Errorf("refusing to remove the execution node %s", name)
		}
		// Configured names are not shell-safe input until validated.
		if !validCommandNode(name) {
			return fmt.Errorf("invalid configured node name %q", name)
		}
		if err := r.guard(ctx); err != nil {
			return err
		}
		// The departing node may already be stopped or unreachable.
		stdout, stderr, stopErr := r.execute(ctx, fmt.Sprintf("/usr/sbin/pcs cluster stop %s --force --request-timeout=300", name))
		if stopErr != nil {
			klog.Infof("Best-effort cluster stop for node %q returned an error; continuing with removal: stdout: %s, stderr: %s, err: %v", name, stdout, stderr, stopErr)
		}
		if err := r.command(ctx, fmt.Sprintf("/usr/sbin/pcs cluster node remove %s --force --skip-offline", name)); err != nil {
			return err
		}
	}
	for _, name := range add {
		var ip string
		for _, n := range r.snapshot.Nodes {
			if n.Name == name {
				ip = n.IP
			}
		}
		if err := r.command(ctx, fmt.Sprintf("/usr/sbin/pcs cluster node add %s addr=%s --force", name, ip)); err != nil {
			return err
		}
		// Checkpoint the added identity before fencing/resource work can fail.
		// The generation claim above keeps the remaining repair work retryable.
		for _, n := range r.snapshot.Nodes {
			if n.Name == name {
				if err := r.command(ctx, fmt.Sprintf("crm_attribute --type crm_config --name tnf_node_uid_%s --update %s", n.Name, n.UID)); err != nil {
					return err
				}
			}
		}
	}
	if err := r.guard(ctx); err != nil {
		return err
	}
	names := make([]string, 0, len(r.snapshot.Nodes))
	mapping := make([]string, 0, len(r.snapshot.Nodes))
	for _, n := range r.snapshot.Nodes {
		names = append(names, n.Name)
		mapping = append(mapping, n.Name+":"+n.IP)
	}
	if err := r.fencing(ctx, r.kube, names); err != nil {
		return err
	}
	if r.continuingRepair {
		if err := r.removeUnstartedMembers(ctx); err != nil {
			return err
		}
		if err := r.command(ctx, fmt.Sprintf("crm_attribute --lifetime reboot --node %s --name force_new_cluster --update %s", r.current, r.repairClaim)); err != nil {
			return err
		}
	}
	currentMapping, err := r.nodeIPMap(ctx)
	if err != nil {
		return err
	}
	if currentMapping != strings.Join(mapping, ";") {
		if err := r.command(ctx, fmt.Sprintf("/usr/sbin/pcs resource update etcd node_ip_map=\"%s\" --wait=300", strings.Join(mapping, ";"))); err != nil {
			return err
		}
	}
	for _, n := range r.snapshot.Nodes {
		if err := r.command(ctx, fmt.Sprintf("crm_attribute --type crm_config --name tnf_node_uid_%s --update %s", n.Name, n.UID)); err != nil {
			return err
		}
	}
	for _, cmd := range []string{"/usr/sbin/pcs cluster enable --all", "/usr/sbin/pcs cluster sync", "/usr/sbin/pcs cluster start --all"} {
		if err := r.command(ctx, cmd); err != nil {
			return err
		}
	}
	if err := r.guard(ctx); err != nil {
		return err
	}
	if err := r.alerts(ctx); err != nil {
		return err
	}
	return r.validate(ctx)
}

func (r *recordedRunner) validate(ctx context.Context) error {
	if err := r.snapshot.MatchesCurrent(ctx, r.kube); err != nil {
		return err
	}
	configured, err := r.configured(ctx)
	if err != nil {
		return err
	}
	remove, add := r.snapshot.MembershipChanges(configured)
	if len(remove)+len(add) > 0 {
		return fmt.Errorf("membership has not converged: remove=%v add=%v", remove, add)
	}
	stdout, _, err := r.execute(ctx, "/usr/sbin/pcs status xml")
	if err != nil {
		return err
	}
	var status pacemaker.PacemakerResult
	if err := xml.Unmarshal([]byte(stdout), &status); err != nil {
		return err
	}
	for _, n := range r.snapshot.Nodes {
		online := false
		for _, actual := range status.Nodes.Node {
			if actual.Name == n.Name && actual.Online == "true" {
				online = true
			}
		}
		if !online {
			return fmt.Errorf("desired node %s is not online yet", n.Name)
		}
	}
	return nil
}

func (r *recordedRunner) removeUnstartedMembers(ctx context.Context) error {
	stdout, _, err := r.execute(ctx, "podman exec etcd /usr/bin/etcdctl member list -w json")
	if err != nil {
		return err
	}
	var members struct {
		Members []struct {
			ID       uint64   `json:"ID"`
			Name     string   `json:"name"`
			PeerURLs []string `json:"peerURLs"`
		} `json:"members"`
	}
	if err := json.Unmarshal([]byte(stdout), &members); err != nil {
		return err
	}
	for _, member := range members.Members {
		if member.Name != "" || len(member.PeerURLs) == 0 {
			continue
		}
		retained := false
		for _, peer := range member.PeerURLs {
			u, err := url.Parse(peer)
			if err != nil {
				return err
			}
			for _, n := range r.snapshot.Nodes {
				if net.ParseIP(u.Hostname()).Equal(net.ParseIP(n.IP)) {
					retained = true
				}
			}
		}
		if !retained {
			if err := r.command(ctx, fmt.Sprintf("podman exec etcd /usr/bin/etcdctl member remove %x", member.ID)); err != nil {
				return err
			}
		}
	}
	return nil
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func validCommandNode(name string) bool { return len(validation.IsDNS1123Subdomain(name)) == 0 }

func (r *recordedRunner) nodeIPMap(ctx context.Context) (string, error) {
	stdout, _, err := r.execute(ctx, "/usr/sbin/pcs cluster cib")
	if err != nil {
		return "", err
	}
	decoder := xml.NewDecoder(strings.NewReader(stdout))
	inside := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch element := token.(type) {
		case xml.StartElement:
			if element.Name.Local == "primitive" {
				for _, a := range element.Attr {
					if a.Name.Local == "id" && a.Value == "etcd" {
						inside = true
					}
				}
			}
			if inside && element.Name.Local == "nvpair" {
				name, value := "", ""
				for _, a := range element.Attr {
					if a.Name.Local == "name" {
						name = a.Value
					}
					if a.Name.Local == "value" {
						value = a.Value
					}
				}
				if name == "node_ip_map" {
					return value, nil
				}
			}
		case xml.EndElement:
			if element.Name.Local == "primitive" {
				inside = false
			}
		}
	}
	return "", fmt.Errorf("etcd node_ip_map is absent from CIB")
}

func (r *recordedRunner) properties(ctx context.Context) (map[string]string, error) {
	stdout, _, err := r.execute(ctx, "/usr/sbin/pcs cluster cib")
	if err != nil {
		return nil, err
	}
	var cib struct {
		XMLName xml.Name `xml:"cib"`
		Sets    []struct {
			Pairs []struct {
				Name  string `xml:"name,attr"`
				Value string `xml:"value,attr"`
			} `xml:"nvpair"`
		} `xml:"configuration>crm_config>cluster_property_set"`
	}
	if err := xml.Unmarshal([]byte(stdout), &cib); err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, set := range cib.Sets {
		for _, pair := range set.Pairs {
			if !strings.HasPrefix(pair.Name, "tnf_") {
				continue
			}
			if _, exists := values[pair.Name]; exists {
				return nil, fmt.Errorf("ambiguous CIB property %s", pair.Name)
			}
			values[pair.Name] = pair.Value
		}
	}
	return values, nil
}
