package etcd

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"

	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"
)

const (
	etcdEndpointNamespace = "openshift-etcd"
	etcdHostEndpointName  = "host-etcd"
	etcdEndpointName      = "etcd"
)

type etcdObserver struct {
	listers        configobservation.Listers
	existingConfig map[string]interface{}
	endpoints      []string
	HealthyMember  map[string]bool

	memberPath  []string
	pendingPath []string

	ObserverdMembers []interface{}
	ObserverdPending []interface{}
	recorder         events.Recorder
}

// TODO break out logic into functions to reduce dupe code.
// ObserveClusterMembers observes the current etcd cluster members.
func ObserveClusterMembers(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
	observedConfig := map[string]interface{}{}
	healthyMember := make(map[string]bool)
	var errs []error

	observer := etcdObserver{
		listers:       genericListers.(configobservation.Listers),
		memberPath:    []string{"cluster", "members"},
		pendingPath:   []string{"cluster", "pending"},
		HealthyMember: healthyMember,
	}

	previouslyObservedMembers, err := observer.getPathObservationData(observer.memberPath, existingConfig)
	if err != nil {
		errs = append(errs, err)
	}

	// etcd-bootstrap is a special case, we make initial assuptions based on the existance of the value in host endpoints
	// once we scale down bootstrap this record should be removed.
	if err := observer.setBootstrapMember(); err != nil {
		errs = append(errs, err)
	}

	if err := observer.setObserverdMembersFromEndpoint(); err != nil {
		errs = append(errs, err)
	}

	if previouslyObservedMembers == nil {
		if len(errs) > 0 {
			return existingConfig, errs
		}
		if len(observer.ObserverdMembers) > 0 {
			if err := unstructured.SetNestedField(observedConfig, observer.ObserverdMembers, observer.memberPath...); err != nil {
				return existingConfig, append(errs, err)
			}
		}
		if !reflect.DeepEqual(previouslyObservedMembers, observer.ObserverdMembers) {
			recorder.Eventf("ObserveClusterMembersUpdated", "Updated cluster members to %v", observer.ObserverdMembers)
		}
		return observedConfig, nil
	}

	previousMembers, err := getMembersFromConfig(previouslyObservedMembers)
	if err != nil {
		errs = append(errs, err)
	}

	for _, previousMember := range previousMembers {
		if observer.HealthyMember[previousMember.Name] || previousMember.Name == "etcd-bootstrap" {
			continue
		}
		_, err := observer.listers.OpenshiftEtcdPodsLister.Pods(etcdEndpointNamespace).Get(previousMember.Name)
		if errors.IsNotFound(err) {
			// verify the node exists
			//TODO this is very opnionated could this come from the endpoint?
			nodeName := strings.TrimPrefix(previousMember.Name, "etcd-member-")

			//TODO this should be a function
			node, err := observer.listers.NodeLister.Get(nodeName)
			if errors.IsNotFound(err) {
				// if the node is no londer available we use the endpoint observatiopn
				klog.Warningf("error: Node %s not found: adding remove status to %s ", nodeName, previousMember.Name)
				clusterMember, err := setMember(previousMember.Name, previousMember.PeerURLS, ceoapi.MemberRemove)
				if err != nil {
					return existingConfig, append(errs, err)

				}
				observer.ObserverdMembers = append(observer.ObserverdMembers, clusterMember)
				continue
			}
			if node.Status.Conditions != nil && node.Status.Conditions[0].Type != "NodeStatusUnknown" || node.Status.Conditions[0].Type != "NodeStatusDown" {
				klog.Warningf("Node Condition not expected %s:", node.Status.Conditions[0].Type)
				clusterMember, err := setMember(previousMember.Name, previousMember.PeerURLS, ceoapi.MemberUnknown)
				if err != nil {
					return existingConfig, append(errs, err)
				}
				observer.ObserverdMembers = append(observer.ObserverdMembers, clusterMember)

				// we dont know why node is not ready but we cant assume we want to scale it
				continue
			}
		}
	}

	if len(errs) > 0 {
		if err := unstructured.SetNestedSlice(observedConfig, previouslyObservedMembers, observer.memberPath...); err != nil {
			return existingConfig, append(errs, err)
		}
		return observedConfig, append(errs, err)
	}

	if len(observer.ObserverdMembers) > 0 {
		if err := unstructured.SetNestedField(observedConfig, observer.ObserverdMembers, observer.memberPath...); err != nil {
			return existingConfig, append(errs, err)
		}
	}

	if !reflect.DeepEqual(previouslyObservedMembers, observer.ObserverdMembers) {
		recorder.Eventf("ObserveClusterMembersUpdated", "Updated cluster members to %v", observer.ObserverdMembers)
	}
	return observedConfig, nil
}

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
			// default to MemberUnknown until we observe further.
			status := ceoapi.MemberUnknown
			etcdURL := map[string]interface{}{}
			name := address.TargetRef.Name

			if err := unstructured.SetNestedField(etcdURL, name, "name"); err != nil {
				return currentConfig, append(errs, err)
			}
			pod, err := listers.OpenshiftEtcdPodsLister.Pods(etcdEndpointNamespace).Get(name)
			if err != nil {
				return currentConfig, append(errs, err)
			}
			if pod.Status.ContainerStatuses[0].State.Waiting != nil && pod.Status.ContainerStatuses[0].State.Waiting.Reason == "CrashLoopBackOff" {
				if isPodCrashLoop(listers, name) {
					status = ceoapi.MemberRemove
				}
				// we are not crashlooping from observation so we are going to clear the member
				status = ceoapi.MemberAdd
			}
			peerURLs := fmt.Sprintf("https://%s:2380", address.IP)
			if err := unstructured.SetNestedField(etcdURL, peerURLs, "peerURLs"); err != nil {
				return currentConfig, append(errs, err)
			}

			if err := unstructured.SetNestedField(etcdURL, string(status), "status"); err != nil {
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

	var observerdClusterMembers []string
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
			observerdClusterMembers = append(observerdClusterMembers, "https://"+address.Hostname+"."+dnsSuffix+":2379")
		}
	}

	if len(observerdClusterMembers) == 0 {
		emptyURLErr := fmt.Errorf("endpoints %s/%s: no etcd endpoint addresses found", etcdEndpointNamespace, etcdHostEndpointName)
		recorder.Warning("ObserveStorageFailed", emptyURLErr.Error())
		errs = append(errs, emptyURLErr)
	}

	if len(errs) > 0 {
		return
	}

	if err := unstructured.SetNestedStringSlice(observedConfig, observerdClusterMembers, storageConfigURLsPath...); err != nil {
		errs = append(errs, err)
		return
	}

	if !reflect.DeepEqual(currentEtcdURLs, observerdClusterMembers) {
		recorder.Eventf("ObserveStorageUpdated", "Updated storage urls to %s", strings.Join(observerdClusterMembers, ","))
	}

	return
}

func (e *etcdObserver) getPathObservationData(path []string, existingConfig map[string]interface{}) ([]interface{}, error) {
	data, _, err := unstructured.NestedSlice(existingConfig, path...)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func isPodCrashLoop(listers configobservation.Listers, name string) bool {
	restartCount := make(map[string]int32)
	timeout := 315 * time.Second
	interval := 5 * time.Second

	// check if the pod is activly crashlooping we check etcd-member only at this time
	if err := wait.PollImmediate(interval, timeout, func() (bool, error) {
		pod, err := listers.OpenshiftEtcdPodsLister.Pods(etcdEndpointNamespace).Get(name)
		if err != nil {
			klog.Errorf("unable to find pod %s: %v. Retrying.", name, err)
			return false, nil
		}
		for _, containerStatus := range pod.Status.ContainerStatuses {
			if containerStatus.State.Waiting == nil || containerStatus.Name != "etcd-member" {
				klog.Warningf("isPodCrashLoop: skipping\n")
				continue
			}
			if restartCount[containerStatus.Name] == 0 {
				klog.Warningf("isPodCrashLoop: was 0 now %v\n", containerStatus.RestartCount)
				restartCount[containerStatus.Name] = containerStatus.RestartCount
			}

			if containerStatus.State.Waiting.Reason == "CrashLoopBackOff" {
				klog.Warningf("isPodCrashLoop: container recount %v observed count %v\n", containerStatus.RestartCount, restartCount[containerStatus.Name])
				if restartCount[containerStatus.Name] > 0 && containerStatus.RestartCount > restartCount[containerStatus.Name] {
					klog.Warningf("found container %s actively in CrashLoopBackOff\n", containerStatus.Name)
					return true, nil
				}
				klog.Warningf("isPodCrashLoop: restartCount[containerStatus.Name] %v\n", containerStatus.RestartCount)
				restartCount[containerStatus.Name] = containerStatus.RestartCount
				// return false, nil
			}
		}
		return false, nil
	}); err != nil {
		klog.Warningf("isPodCrashLoop: returning FALSE\n")
		return false
	}

	klog.Warningf("isPodCrashLoop: returning TRUE\n")
	return true
}

func (e *etcdObserver) setBootstrapMember() error {
	endpoints, err := e.listers.OpenshiftEtcdEndpointsLister.Endpoints(etcdEndpointNamespace).Get(etcdHostEndpointName)
	if errors.IsNotFound(err) {
		e.recorder.Warningf("setBootstrapMember", "Required %s/%s endpoint not found", etcdEndpointNamespace, etcdHostEndpointName)
		return err
	}
	if err != nil {
		e.recorder.Warningf("setBootstrapMember", "Error getting %s/%s endpoint: %v", etcdEndpointNamespace, etcdHostEndpointName, err)
		return err
	}
	dnsSuffix := endpoints.Annotations["alpha.installer.openshift.io/dns-suffix"]
	if len(dnsSuffix) == 0 {
		err := fmt.Errorf("endpoints %s/%s: alpha.installer.openshift.io/dns-suffix annotation not found", etcdEndpointNamespace, etcdHostEndpointName)
		e.recorder.Warning("setBootstrapMember", err.Error())
		return err
	}

	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.Hostname == "etcd-bootstrap" {
				name := address.Hostname
				peerURLs := fmt.Sprintf("https://%s.%s:2380", name, dnsSuffix)
				clusterMember, err := setMember(name, []string{peerURLs}, ceoapi.MemberUnknown)
				if err != nil {
					return err
				}
				e.ObserverdMembers = append(e.ObserverdMembers, clusterMember)
			}
		}
	}
	return nil
}

func (e *etcdObserver) setObserverdMembersFromEndpoint() error {
	endpoints, err := e.listers.OpenshiftEtcdEndpointsLister.Endpoints(etcdEndpointNamespace).Get(etcdEndpointName)
	if errors.IsNotFound(err) {
		e.recorder.Warningf("ObserveClusterMembers", "Required %s/%s endpoint not found", etcdEndpointNamespace, etcdHostEndpointName)
		return err
	}
	if err != nil {
		e.recorder.Warningf("ObserveClusterMembers", "Error getting %s/%s endpoint: %v", etcdEndpointNamespace, etcdHostEndpointName, err)
		return err
	}

	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			name := address.TargetRef.Name
			peerURLs := fmt.Sprintf("https://%s:2380", address.IP)
			clusterMember, err := setMember(name, []string{peerURLs}, ceoapi.MemberReady)
			if err != nil {
				return err
			}

			e.HealthyMember[name] = true
			e.ObserverdMembers = append(e.ObserverdMembers, clusterMember)
		}
	}
	return nil
}

func (e *etcdObserver) setObserverdPendingFromEndpoint() error {
	status := ceoapi.MemberAdd

	endpoints, err := e.listers.OpenshiftEtcdEndpointsLister.Endpoints(etcdEndpointNamespace).Get(etcdEndpointName)
	if errors.IsNotFound(err) {
		e.recorder.Warningf("ObserveClusterPending", "Required %s/%s endpoint not found", etcdEndpointNamespace, etcdHostEndpointName)
		return err
	}
	if err != nil {
		e.recorder.Warningf("ObserveClusterPending", "Error getting %s/%s endpoint: %v", etcdEndpointNamespace, etcdHostEndpointName, err)
		return err
	}

	for _, subset := range endpoints.Subsets {
		for _, address := range subset.NotReadyAddresses {
			name := address.TargetRef.Name
			pod, err := e.listers.OpenshiftEtcdPodsLister.Pods(etcdEndpointNamespace).Get(name)
			if err != nil {
				return err
			}
			if pod.Status.ContainerStatuses[0].State.Waiting != nil && pod.Status.ContainerStatuses[0].State.Waiting.Reason == "CrashLoopBackOff" {
				if isPodCrashLoop(e.listers, name) {
					status = ceoapi.MemberRemove
				}
			}
			peerURLs := fmt.Sprintf("https://%s:2380", address.IP)
			clusterMember, err := setMember(name, []string{peerURLs}, status)
			if err != nil {
				return err
			}

			e.ObserverdPending = append(e.ObserverdPending, clusterMember)
		}
	}
	return nil
}

func (e *etcdObserver) isPendingRemoval(members ceoapi.Member, existingConfig map[string]interface{}) (bool, error) {
	previousPendingObserved, found, err := unstructured.NestedSlice(existingConfig, e.pendingPath...)
	if err != nil {
		return false, err
	}
	if found {
		previousPending, err := getMembersFromConfig(previousPendingObserved)
		if err != nil {
			return false, err
		}

		for _, pendingMember := range previousPending {
			if pendingMember.Conditions == nil {
				return false, nil
			}
			if pendingMember.Name == members.Name && pendingMember.Conditions[0].Type == ceoapi.MemberRemove {
				return true, nil
			}
		}
	}
	return false, nil
}

//TODO move to util
func getMembersFromConfig(config []interface{}) ([]ceoapi.Member, error) {
	var members []ceoapi.Member
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

		condition := ceoapi.GetMemberCondition(status)
		m := ceoapi.Member{
			Name:     name,
			PeerURLS: []string{peerURLs},
			Conditions: []ceoapi.MemberCondition{
				{
					Type: condition,
				},
			},
		}
		members = append(members, m)
	}
	return members, nil
}

func setMember(name string, peerURLs []string, status ceoapi.MemberConditionType) (interface{}, error) {
	etcdURL := map[string]interface{}{}
	if err := unstructured.SetNestedField(etcdURL, name, "name"); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(etcdURL, peerURLs[0], "peerURLs"); err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(etcdURL, string(status), "status"); err != nil {
		return nil, err
	}
	return etcdURL, nil
}
