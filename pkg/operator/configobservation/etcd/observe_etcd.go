package etcd

import (
	"fmt"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog"

	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
)

const (
	etcdEndpointNamespace = "openshift-etcd"
	etcdHostEndpointName  = "host-etcd"
	etcdEndpointName      = "etcd"
)

// TODO break out logic into functions to reduce dupe code.
// ObserveClusterMembers observes the current etcd cluster members.
func ObserveClusterMembers(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
	listers := genericListers.(configobservation.Listers)
	observedConfig := map[string]interface{}{}
	memberPath := []string{"cluster", "members"}
	pendingPath := []string{"cluster", "pending"}
	var errs []error

	previousMembersObserved, found, err := unstructured.NestedSlice(existingConfig, memberPath...)
	if err != nil {
		errs = append(errs, err)
	}
	previousMembers, err := getMembersFromConfig(previousMembersObserved)
	if err != nil {
		errs = append(errs, err)
	}

	previousPendingObserved, _, err := unstructured.NestedSlice(existingConfig, pendingPath...)
	if err != nil {
		errs = append(errs, err)
	}
	previousPending, err := getMembersFromConfig(previousPendingObserved)
	if err != nil {
		errs = append(errs, err)
	}
	var etcdURLs []interface{}

	// etcd-bootstrap is a special case, we make initial assuptions based on the existance of the value in host endpoints
	// once we scale down bootstrap this record should be removed.
	bootstrapMember, err := setBootstrapMember(listers, etcdURLs, recorder)
	if err != nil {
		return existingConfig, append(errs, err)
	}
	etcdURLs = append(etcdURLs, bootstrapMember)

	endpoints, err := listers.OpenshiftEtcdEndpointsLister.Endpoints(etcdEndpointNamespace).Get(etcdEndpointName)
	if errors.IsNotFound(err) {
		recorder.Warningf("ObserveClusterMembers", "Required %s/%s endpoint not found", etcdEndpointNamespace, etcdHostEndpointName)
		return existingConfig, append(errs, err)
	}
	if err != nil {
		recorder.Warningf("ObserveClusterMembers", "Error getting %s/%s endpoint: %v", etcdEndpointNamespace, etcdHostEndpointName, err)
		return existingConfig, append(errs, err)
	}

	healthyMember := make(map[string]bool)
	// happy members
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			name := address.TargetRef.Name
			peerURLs := fmt.Sprintf("https://%s:2380", address.IP)
			etcdURL, err := setMember(name, []string{peerURLs}, clustermembercontroller.MemberReady)
			if err != nil {
				return existingConfig, append(errs, err)
			}

			healthyMember[name] = true
			etcdURLs = append(etcdURLs, etcdURL)
		}
	}
	for _, previousMember := range previousMembers {
		// if we are pending removal that means we have been removed from etcd cluster so we a re no longer listed here
		// if we are healthy then we are already here :)
		if healthyMember[previousMember.Name] || isPendingRemoval(previousMember, previousPending) {
			continue
		}
		if previousMember.Name != "etcd-bootstrap" {
			etcdPod, err := listers.OpenshiftEtcdPodsLister.Pods(etcdEndpointNamespace).Get(previousMember.Name)
			if errors.IsNotFound(err) {
				// verify the node exists
				//TODO this is very opnionated could this come from the endpoint?
				nodeName := strings.TrimPrefix(previousMember.Name, "etcd-member-")

				//TODO this should be a function
				node, err := listers.NodeLister.Get(nodeName)
				if errors.IsNotFound(err) {
					// if the node is no londer available we use the endpoint observatiopn
					klog.Warningf("error: Node %s not found: adding remove status to %s ", nodeName, previousMember.Name)
					etcdURL, err := setMember(previousMember.Name, previousMember.PeerURLS, clustermembercontroller.MemberRemove)
					if err != nil {
						return existingConfig, append(errs, err)
					}
					etcdURLs = append(etcdURLs, etcdURL)
					continue
				}
				if node.Status.Conditions != nil && node.Status.Conditions[0].Type != "NodeStatusUnknown" || node.Status.Conditions[0].Type != "NodeStatusDown" {
					klog.Warningf("Node Condition not expected %s:", node.Status.Conditions[0].Type)
					etcdURL, err := setMember(previousMember.Name, previousMember.PeerURLS, clustermembercontroller.MemberUnknown)
					if err != nil {
						return existingConfig, append(errs, err)
					}
					etcdURLs = append(etcdURLs, etcdURL)

					// we dont know why node is not ready but we cant assume we want to scale it
					continue
				}
			}

			// since the pod exists lets figure out if the endpoint being down is a result of etcd crashlooping
			if etcdPod.Status.ContainerStatuses[0].State.Waiting != nil && etcdPod.Status.ContainerStatuses[0].State.Waiting.Reason == "CrashLoopBackOff" {
				etcdURL, err := setMember(previousMember.Name, previousMember.PeerURLS, clustermembercontroller.MemberRemove)
				if err != nil {
					return existingConfig, append(errs, err)
				}
				etcdURLs = append(etcdURLs, etcdURL)
				continue
			}
		}
	}

	if len(errs) > 0 {
		if found {
			if err := unstructured.SetNestedSlice(observedConfig, previousMembersObserved, memberPath...); err != nil {
				errs = append(errs, err)
			}
		}
		return existingConfig, errs
	}

	if len(etcdURLs) > 0 {
		if err := unstructured.SetNestedField(observedConfig, etcdURLs, memberPath...); err != nil {
			return existingConfig, append(errs, err)
		}
	}

	if !reflect.DeepEqual(previousMembersObserved, etcdURLs) {
		recorder.Eventf("ObserveClusterMembersUpdated", "Updated cluster members to %v", etcdURLs)
	}
	return observedConfig, nil
}

// ObservePendingClusterMembers observes pending etcd cluster members who are atempting to join the cluster.
// TODO it is possible that a member which is part of the cluster can show pending status if Pod goes down. We need to handle this flapping.
// If you are member you should not renturn to pending. During bootstrap this isn't a fatal flaw but its not optimal.
func ObservePendingClusterMembers(genericListers configobserver.Listers, recorder events.Recorder, currentConfig map[string]interface{}) (observedConfig map[string]interface{}, errs []error) {
	listers := genericListers.(configobservation.Listers)
	observedConfig = map[string]interface{}{}
	clusterMemberPath := []string{"cluster", "pending"}

	currentClusterMembers, found, err := unstructured.NestedSlice(currentConfig, clusterMemberPath...)
	if err != nil {
		errs = append(errs, err)
	}

	var etcdURLs []interface{}
	etcdEndpoints, err := listers.OpenshiftEtcdEndpointsLister.Endpoints(etcdEndpointNamespace).Get(etcdEndpointName)
	if errors.IsNotFound(err) {
		recorder.Warningf("ObservePendingClusterMembers", "Required %s/%s endpoint not found", etcdEndpointNamespace, etcdEndpointName)
		errs = append(errs, fmt.Errorf("endpoints/etcd.openshift-etcd: not found"))
		return
	}
	if err != nil {
		recorder.Warningf("ObservePendingClusterMembers", "Error getting %s/%s endpoint: %v", etcdEndpointNamespace, etcdEndpointName, err)
		errs = append(errs, err)
		return
	}
	for _, subset := range etcdEndpoints.Subsets {
		for _, address := range subset.NotReadyAddresses {
			etcdURL := map[string]interface{}{}
			name := address.TargetRef.Name
			if err := unstructured.SetNestedField(etcdURL, name, "name"); err != nil {
				return currentConfig, append(errs, err)
			}

			peerURLs := fmt.Sprintf("https://%s:2380", address.IP)
			if err := unstructured.SetNestedField(etcdURL, peerURLs, "peerURLs"); err != nil {
				return currentConfig, append(errs, err)
			}
			etcdURLs = append(etcdURLs, etcdURL)
		}
	}

	if len(etcdURLs) > 0 {
		if err := unstructured.SetNestedField(observedConfig, etcdURLs, clusterMemberPath...); err != nil {
			return currentConfig, append(errs, err)
		}
	}

	if len(errs) > 0 {
		if found {
			if err := unstructured.SetNestedSlice(observedConfig, currentClusterMembers, clusterMemberPath...); err != nil {
				errs = append(errs, err)
			}
		}
		return
	}

	if !reflect.DeepEqual(currentClusterMembers, etcdURLs) {
		recorder.Eventf("ObservePendingClusterMembersUpdated", "Updated pending cluster members to %v", etcdURLs)
	}
	return
}

// ObserveStorageURLs observes the storage config URLs. If there is a problem observing the current storage config URLs,
// then the previously observed storage config URLs will be re-used.
func ObserveStorageURLs(genericListers configobserver.Listers, recorder events.Recorder, currentConfig map[string]interface{}) (observedConfig map[string]interface{}, errs []error) {
	listers := genericListers.(configobservation.Listers)
	observedConfig = map[string]interface{}{}
	storageConfigURLsPath := []string{"storageConfig", "urls"}

	currentEtcdURLs, found, err := unstructured.NestedStringSlice(currentConfig, storageConfigURLsPath...)
	if err != nil {
		errs = append(errs, err)
	}
	if found {
		if err := unstructured.SetNestedStringSlice(observedConfig, currentEtcdURLs, storageConfigURLsPath...); err != nil {
			errs = append(errs, err)
		}
	}

	var etcdURLs []string
	etcdEndpoints, err := listers.OpenshiftEtcdEndpointsLister.Endpoints(etcdEndpointNamespace).Get(etcdHostEndpointName)
	if errors.IsNotFound(err) {
		recorder.Warningf("ObserveStorageFailed", "Required %s/%s endpoint not found", etcdEndpointNamespace, etcdHostEndpointName)
		errs = append(errs, fmt.Errorf("endpoints/host-etcd.openshift-etcd: not found"))
		return
	}
	if err != nil {
		recorder.Warningf("ObserveStorageFailed", "Error getting %s/%s endpoint: %v", etcdEndpointNamespace, etcdHostEndpointName, err)
		errs = append(errs, err)
		return
	}
	dnsSuffix := etcdEndpoints.Annotations["alpha.installer.openshift.io/dns-suffix"]
	if len(dnsSuffix) == 0 {
		dnsErr := fmt.Errorf("endpoints %s/%s: alpha.installer.openshift.io/dns-suffix annotation not found", etcdEndpointNamespace, etcdHostEndpointName)
		recorder.Warning("ObserveStorageFailed", dnsErr.Error())
		errs = append(errs, dnsErr)
		return
	}
	for subsetIndex, subset := range etcdEndpoints.Subsets {
		for addressIndex, address := range subset.Addresses {
			if address.Hostname == "" {
				addressErr := fmt.Errorf("endpoints %s/%s: subsets[%v]addresses[%v].hostname not found", etcdHostEndpointName, etcdEndpointNamespace, subsetIndex, addressIndex)
				recorder.Warningf("ObserveStorageFailed", addressErr.Error())
				errs = append(errs, addressErr)
				continue
			}
			etcdURLs = append(etcdURLs, "https://"+address.Hostname+"."+dnsSuffix+":2379")
		}
	}

	if len(etcdURLs) == 0 {
		emptyURLErr := fmt.Errorf("endpoints %s/%s: no etcd endpoint addresses found", etcdEndpointNamespace, etcdHostEndpointName)
		recorder.Warning("ObserveStorageFailed", emptyURLErr.Error())
		errs = append(errs, emptyURLErr)
	}

	if len(errs) > 0 {
		return
	}

	if err := unstructured.SetNestedStringSlice(observedConfig, etcdURLs, storageConfigURLsPath...); err != nil {
		errs = append(errs, err)
		return
	}

	if !reflect.DeepEqual(currentEtcdURLs, etcdURLs) {
		recorder.Eventf("ObserveStorageUpdated", "Updated storage urls to %s", strings.Join(etcdURLs, ","))
	}

	return
}

//TODO move to util
func getMembersFromConfig(config []interface{}) ([]clustermembercontroller.Member, error) {
	var members []clustermembercontroller.Member
	for _, member := range config {
		memberMap, _ := member.(map[string]interface{})
		name, exists, err := unstructured.NestedString(memberMap, "name")
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("member name does not exist")
		}
		peerURLs, exists, err := unstructured.NestedString(memberMap, "peerURLs")
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("member peerURLs do not exist")
		}

		status, exists, err := unstructured.NestedString(memberMap, "status")
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("member status does not exist")
		}

		condition := clustermembercontroller.GetMemberCondition(status)
		m := clustermembercontroller.Member{
			Name:     name,
			PeerURLS: []string{peerURLs},
			Conditions: []clustermembercontroller.MemberCondition{
				{
					Type: condition,
				},
			},
		}
		members = append(members, m)
	}
	return members, nil
}

func setBootstrapMember(listers configobservation.Listers, etcdURLs []interface{}, recorder events.Recorder) ([]interface{}, error) {
	endpoints, err := listers.OpenshiftEtcdEndpointsLister.Endpoints(etcdEndpointNamespace).Get(etcdHostEndpointName)
	if errors.IsNotFound(err) {
		recorder.Warningf("setBootstrapMember", "Required %s/%s endpoint not found", etcdEndpointNamespace, etcdHostEndpointName)
		return nil, err
	}
	if err != nil {
		recorder.Warningf("setBootstrapMember", "Error getting %s/%s endpoint: %v", etcdEndpointNamespace, etcdHostEndpointName, err)
		return nil, err
	}
	dnsSuffix := endpoints.Annotations["alpha.installer.openshift.io/dns-suffix"]
	if len(dnsSuffix) == 0 {
		err := fmt.Errorf("endpoints %s/%s: alpha.installer.openshift.io/dns-suffix annotation not found", etcdEndpointNamespace, etcdHostEndpointName)
		recorder.Warning("setBootstrapMember", err.Error())
		return nil, err
	}

	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.Hostname == "etcd-bootstrap" {
				name := address.Hostname
				peerURLs := fmt.Sprintf("https://%s.%s:2380", name, dnsSuffix)
				etcdURL, err := setMember(name, []string{peerURLs}, clustermembercontroller.MemberUnknown)
				if err != nil {
					return nil, err
				}
				etcdURLs = append(etcdURLs, etcdURL)
			}
		}
	}
	return etcdURLs, nil
}

func setMember(name string, peerURLs []string, status clustermembercontroller.MemberConditionType) (map[string]interface{}, error) {
	etcdURL := map[string]interface{}{}
	if err := unstructured.SetNestedField(etcdURL, name, "name"); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(etcdURL, peerURLs[0], "peerURLs"); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(etcdURL, status, "status"); err != nil {
		return nil, err
	}
	return etcdURL, nil
}

func isPendingRemoval(members clustermembercontroller.Member, pending []clustermembercontroller.Member) bool {
	for _, pendingMember := range pending {
		if pendingMember.Conditions == nil {
			return false
		}
		if pendingMember.Name == members.Name && pendingMember.Conditions[0].Type == clustermembercontroller.MemberRemove {
			return true
		}
	}
	return false
}
