package etcd

import (
	"fmt"
	"net"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
)

const (
	etcdEndpointNamespace = "openshift-etcd"
	etcdHostEndpointName  = "host-etcd"
	etcdEndpointName      = "etcd"
)

// TODO break out logic into functions to reduce dupe code.
// ObserveClusterMembers observes the current etcd cluster members.
func ObserveClusterMembers(genericListers configobserver.Listers, recorder events.Recorder, currentConfig map[string]interface{}) (observedConfig map[string]interface{}, errs []error) {
	listers := genericListers.(configobservation.Listers)
	observedConfig = map[string]interface{}{}
	clusterMemberPath := []string{"cluster", "members"}

	currentClusterMembers, found, err := unstructured.NestedSlice(currentConfig, clusterMemberPath...)
	if err != nil {
		errs = append(errs, err)
	}
	if found {
		if err := unstructured.SetNestedSlice(observedConfig, currentClusterMembers, clusterMemberPath...); err != nil {
			errs = append(errs, err)
		}
	}

	previousMemberCount := len(currentClusterMembers)
	var etcdURLs []interface{}
	// etcd-bootstrao is a special case. In the future the bootstrap node will be observable but for now we make assumptions.
	etcdHostEndpoints, err := listers.OpenshiftEtcdEndpointsLister.Endpoints(etcdEndpointNamespace).Get(etcdHostEndpointName)
	if errors.IsNotFound(err) {
		recorder.Warningf("ObserveClusterMembers", "Required %s/%s endpoint not found", etcdEndpointNamespace, etcdHostEndpointName)
		errs = append(errs, fmt.Errorf("endpoints/host-etcd.openshift-etcd: not found"))
		return
	}
	if err != nil {
		recorder.Warningf("ObserveClusterMembers", "Error getting %s/%s endpoint: %v", etcdEndpointNamespace, etcdHostEndpointName, err)
		errs = append(errs, err)
		return
	}
	dnsSuffix := etcdHostEndpoints.Annotations["alpha.installer.openshift.io/dns-suffix"]
	if len(dnsSuffix) == 0 {
		dnsErr := fmt.Errorf("endpoints %s/%s: alpha.installer.openshift.io/dns-suffix annotation not found", etcdEndpointNamespace, etcdHostEndpointName)
		recorder.Warning("ObserveClusterMembersFailed", dnsErr.Error())
		errs = append(errs, dnsErr)
		return
	}
	currentMemberCount := 0
	for _, subset := range etcdHostEndpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.Hostname == "etcd-bootstrap" {
				etcdURL := map[string]interface{}{}
				ip, err := net.LookupIP(fmt.Sprintf("%s.%s", address.Hostname, dnsSuffix))
				if err != nil {
					errs = append(errs, err)
				}
				name := address.Hostname
				if err := unstructured.SetNestedField(etcdURL, name, "name"); err != nil {
					return currentConfig, append(errs, err)
				}
				peerURLs := fmt.Sprintf("https://%s:2380", ip[0].String())
				if err := unstructured.SetNestedField(etcdURL, peerURLs, "peerURLs"); err != nil {
					return currentConfig, append(errs, err)
				}
				currentMemberCount++
				etcdURLs = append(etcdURLs, etcdURL)
			}
		}
	}

	// TODO handle flapping if the member was listed and is now not available then we keep the old value.
	// membership removal requires a seperate observastion method, perhaps metrics. A finalizer could handle
	// removal but we also need to be aware that an admin can jump in do what they please.
	etcdEndpoints, err := listers.OpenshiftEtcdEndpointsLister.Endpoints(etcdEndpointNamespace).Get(etcdEndpointName)
	if errors.IsNotFound(err) {
		recorder.Warningf("ObserveClusterMembers", "Required %s/%s endpoint not found", etcdEndpointNamespace, etcdEndpointName)
		errs = append(errs, fmt.Errorf("endpoints/etcd.openshift-etcd: not found"))
		return
	}
	if err != nil {
		recorder.Warningf("ObserveClusterMembers", "Error getting %s/%s endpoint: %v", etcdEndpointNamespace, etcdEndpointName, err)
		errs = append(errs, err)
		return
	}
	for _, subset := range etcdEndpoints.Subsets {
		for _, address := range subset.Addresses {
			etcdURL := map[string]interface{}{}
			name := address.TargetRef.Name
			if err := unstructured.SetNestedField(etcdURL, name, "name"); err != nil {
				return currentConfig, append(errs, err)
			}

			peerURLs := fmt.Sprintf("https://%s:2380", address.IP)
			if err := unstructured.SetNestedField(etcdURL, peerURLs, "peerURLs"); err != nil {
				return currentConfig, append(errs, err)
			}
			currentMemberCount++
			etcdURLs = append(etcdURLs, etcdURL)
		}
	}

	if currentMemberCount >= previousMemberCount {
		if err := unstructured.SetNestedField(observedConfig, etcdURLs, clusterMemberPath...); err != nil {
			return currentConfig, append(errs, err)
		}
	} else {
		// for now we don't allow the list to deciment because we are only handling bootstrap
		// in future this needs proper consideration.
		recorder.Warningf("ObserveClusterMembers", "Possible flapping current members observed (%d) is less than previous (%v)", currentMemberCount, previousMemberCount)
		return currentConfig, errs
	}

	if len(errs) > 0 {
		return
	}

	if !reflect.DeepEqual(currentClusterMembers, etcdURLs) {
		recorder.Eventf("ObserveClusterMembersUpdated", "Updated cluster members to %v", etcdURLs)
	}
	return
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
	if found {
		if err := unstructured.SetNestedSlice(observedConfig, currentClusterMembers, clusterMemberPath...); err != nil {
			errs = append(errs, err)
		}
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
