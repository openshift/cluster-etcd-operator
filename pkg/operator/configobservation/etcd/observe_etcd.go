package etcd

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/log"
	ceoapi "github.com/openshift/cluster-etcd-operator/pkg/operator/api"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	corelistersv1 "k8s.io/client-go/listers/core/v1"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/configobservation"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog"

	"github.com/openshift/library-go/pkg/operator/configobserver"
	"github.com/openshift/library-go/pkg/operator/events"
)

const (
	Pending = "pending"
	Member  = "member"
)

type etcdObserver struct {
	listers        configobservation.Listers
	existingConfig map[string]interface{}
	endpoints      []string
	HealthyMember  map[string]bool
	ClusterDomain  string

	memberPath  []string
	pendingPath []string

	ObservedMembers           []interface{}
	ObservedPending           []interface{}
	PreviouslyObservedPending []ceoapi.Member
	recorder                  events.Recorder
}

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
	if err := observer.setClusterDomain(); err != nil {
		return existingConfig, append(errs, err)
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

	if err := observer.setObservedEtcdFromEndpoint("members"); err != nil {
		errs = append(errs, err)
	}

	if previouslyObservedMembers == nil {
		if len(errs) > 0 {
			return existingConfig, errs
		}
		if len(observer.ObservedMembers) > 0 {
			if err := unstructured.SetNestedField(observedConfig, observer.ObservedMembers, observer.memberPath...); err != nil {
				return existingConfig, append(errs, err)
			}
		}
		if !reflect.DeepEqual(previouslyObservedMembers, observer.ObservedMembers) {
			recorder.Eventf("ObserveClusterMembersUpdated", "Updated cluster members to %v", observer.ObservedMembers)
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
		_, err := observer.listers.OpenshiftEtcdPodsLister.Pods(clustermembercontroller.EtcdEndpointNamespace).Get(previousMember.Name)
		if errors.IsNotFound(err) {
			// verify the node exists
			//TODO this is very opnionated could this come from the endpoint?
			nodeName := strings.TrimPrefix(previousMember.Name, "etcd-member-")

			_, err := observer.listers.NodeLister.Get(nodeName)
			if errors.IsNotFound(err) {
				// if the node is no londer available we use the endpoint observatiopn
				klog.Warningf("error: Node %s not found: adding remove status to %s ", nodeName, previousMember.Name)
				clusterMember, err := setMember(previousMember.Name, previousMember.PeerURLS, ceoapi.MemberRemove)
				if err != nil {
					return existingConfig, append(errs, err)

				}
				observer.ObservedMembers = append(observer.ObservedMembers, clusterMember)
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

	if len(observer.ObservedMembers) > 0 {
		if err := unstructured.SetNestedField(observedConfig, observer.ObservedMembers, observer.memberPath...); err != nil {
			return existingConfig, append(errs, err)
		}
	}

	if !reflect.DeepEqual(previouslyObservedMembers, observer.ObservedMembers) {
		recorder.Eventf("ObserveClusterMembersUpdated", "Updated cluster members to %v", observer.ObservedMembers)
	}
	return observedConfig, nil
}

func ObservePendingClusterMembers(genericListers configobserver.Listers, recorder events.Recorder, existingConfig map[string]interface{}) (map[string]interface{}, []error) {
	observedConfig := map[string]interface{}{}
	healthyMember := make(map[string]bool)
	var errs []error

	observer := etcdObserver{
		listers:       genericListers.(configobservation.Listers),
		memberPath:    []string{"cluster", "members"},
		pendingPath:   []string{"cluster", "pending"},
		HealthyMember: healthyMember,
	}
	if err := observer.setClusterDomain(); err != nil {
		return existingConfig, append(errs, err)
	}
	previouslyObservedPending, err := observer.getPathObservationData(observer.pendingPath, existingConfig)
	if err != nil {
		errs = append(errs, err)
	}
	// order is important this is needed before setObserved
	previousPending, err := getMembersFromConfig(previouslyObservedPending)
	if err != nil {
		errs = append(errs, err)
	}
	observer.PreviouslyObservedPending = previousPending

	if err := observer.setObservedEtcdFromEndpoint("pending"); err != nil {
		errs = append(errs, err)
	}

	if len(observer.ObservedPending) > 0 {
		if err := unstructured.SetNestedField(observedConfig, observer.ObservedPending, observer.pendingPath...); err != nil {
			klog.Errorf("etcdURLs > 0 ERRRPRRRRRR: %v", errs)
			return existingConfig, append(errs, err)
		}
	}

	if len(errs) > 0 {
		if previouslyObservedPending != nil {
			if err := unstructured.SetNestedSlice(observedConfig, observer.ObservedPending, observer.pendingPath...); err != nil {
				errs = append(errs, err)
			}
		}
		return existingConfig, append(errs, err)
	}

	if !reflect.DeepEqual(previouslyObservedPending, observer.ObservedPending) {
		recorder.Eventf("ObservePendingClusterMembersUpdated", "Updated pending cluster members to %v", observer.ObservedPending)
	}
	return observedConfig, nil
}

func isPendingReady(bucket string, podName string, scalingName string, podLister corelistersv1.PodLister) bool {
	if bucket == "pending" && podName != scalingName {
		return false
	}
	pod, err := podLister.Pods(clustermembercontroller.EtcdEndpointNamespace).Get(podName)
	if err != nil {
		klog.Errorf("isPendingReady: error getting pod %#v", err)
		return false
	}
	if len(pod.Status.InitContainerStatuses) < 2 {
		klog.Infof("isPendingReady: waiting for init cert containers to pass")
		return false
	}
	if pod.Status.InitContainerStatuses[1].State.Terminated != nil && pod.Status.InitContainerStatuses[1].State.Terminated.ExitCode == 0 {
		if pod.Status.ContainerStatuses[0].State.Waiting == nil {
			klog.Info("isPendingReady: the container is either running/crashlooping")
			return false
		}
		return true
	}

	return false
}

func isPendingAdd(bucket string, podName string, previousPending []ceoapi.Member, scalingName string) bool {
	if bucket == "members" {
		return false
	}
	for _, previous := range previousPending {
		// we observe previously Ready as a condition to eval Add
		if previous.Conditions != nil {
			if previous.Name == podName && previous.Conditions[0].Type == ceoapi.MemberReady {
				log.Infof("checking Pod name %s vs CM name %s", podName, scalingName)
				if podName == scalingName {
					return true
				}
			}
		}
	}
	return false

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
	etcdEndpoints, err := listers.OpenshiftEtcdEndpointsLister.Endpoints(clustermembercontroller.EtcdEndpointNamespace).Get(clustermembercontroller.EtcdHostEndpointName)
	if errors.IsNotFound(err) {
		recorder.Warningf("ObserveStorageFailed", "Required %s/%s endpoint not found", clustermembercontroller.EtcdEndpointNamespace, clustermembercontroller.EtcdHostEndpointName)
		errs = append(errs, fmt.Errorf("endpoints/host-etcd.openshift-etcd: not found"))
		return
	}
	if err != nil {
		recorder.Warningf("ObserveStorageFailed", "Error getting %s/%s endpoint: %v", clustermembercontroller.EtcdEndpointNamespace, clustermembercontroller.EtcdHostEndpointName, err)
		errs = append(errs, err)
		return
	}
	dnsSuffix := etcdEndpoints.Annotations["alpha.installer.openshift.io/dns-suffix"]
	if len(dnsSuffix) == 0 {
		dnsErr := fmt.Errorf("endpoints %s/%s: alpha.installer.openshift.io/dns-suffix annotation not found", clustermembercontroller.EtcdEndpointNamespace, clustermembercontroller.EtcdHostEndpointName)
		recorder.Warning("ObserveStorageFailed", dnsErr.Error())
		errs = append(errs, dnsErr)
		return
	}
	for subsetIndex, subset := range etcdEndpoints.Subsets {
		for addressIndex, address := range subset.Addresses {
			if address.Hostname == "" {
				addressErr := fmt.Errorf("endpoints %s/%s: subsets[%v]addresses[%v].hostname not found", clustermembercontroller.EtcdHostEndpointName, clustermembercontroller.EtcdEndpointNamespace, subsetIndex, addressIndex)
				recorder.Warningf("ObserveStorageFailed", addressErr.Error())
				errs = append(errs, addressErr)
				continue
			}
			observerdClusterMembers = append(observerdClusterMembers, "https://"+address.Hostname+"."+dnsSuffix+":2379")
		}
	}

	if len(observerdClusterMembers) == 0 {
		emptyURLErr := fmt.Errorf("endpoints %s/%s: no etcd endpoint addresses found", clustermembercontroller.EtcdEndpointNamespace, clustermembercontroller.EtcdHostEndpointName)
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

func isPodCrashLoop(bucket string, pod *corev1.Pod) bool {
	if bucket == "members" {
		return false
	}
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.Name != "etcd-member" {
			continue
		}
		if containerStatus.State.Waiting != nil && containerStatus.State.Waiting.Reason == "CrashLoopBackOff" {
			if containerStatus.LastTerminationState.Terminated != nil {
				for _, cond := range pod.Status.Conditions {
					if cond.Type == "Initialized" && cond.Status == "True" {
						delay := cond.LastTransitionTime.Time.Add(+time.Minute * 5)
						if containerStatus.LastTerminationState.Terminated.FinishedAt.After(delay) {
							klog.Warningf("isPodCrashLoop: pod %s was observed in CrashLoop", pod.Name)
							return true
						}
						return false
					}
				}
			}
		}
	}
	return false
}

func (e *etcdObserver) setClusterDomain() error {
	endpoints, err := e.listers.OpenshiftEtcdEndpointsLister.Endpoints(clustermembercontroller.EtcdEndpointNamespace).Get(clustermembercontroller.EtcdHostEndpointName)
	if errors.IsNotFound(err) {
		e.recorder.Warningf("ObserveClusterPending", "Required %s/%s endpoint not found", clustermembercontroller.EtcdEndpointNamespace, clustermembercontroller.EtcdHostEndpointName)
		return err
	}
	if err != nil {
		e.recorder.Warningf("ObserveClusterPending", "Error getting %s/%s endpoint: %v", clustermembercontroller.EtcdEndpointNamespace, clustermembercontroller.EtcdHostEndpointName, err)
		return err
	}
	clusterDomain := endpoints.Annotations["alpha.installer.openshift.io/dns-suffix"]
	if len(clusterDomain) == 0 {
		err := fmt.Errorf("endpoints %s/%s: alpha.installer.openshift.io/dns-suffix annotation not found", clustermembercontroller.EtcdEndpointNamespace, clustermembercontroller.EtcdHostEndpointName)
		e.recorder.Warning("ObserveClusterMembers", err.Error())
		return err
	}
	e.ClusterDomain = clusterDomain
	return nil
}

func (e *etcdObserver) setBootstrapMember() error {
	endpoints, err := e.listers.OpenshiftEtcdEndpointsLister.Endpoints(clustermembercontroller.EtcdEndpointNamespace).Get(clustermembercontroller.EtcdHostEndpointName)
	if errors.IsNotFound(err) {
		e.recorder.Warningf("setBootstrapMember", "Required %s/%s endpoint not found", clustermembercontroller.EtcdEndpointNamespace, clustermembercontroller.EtcdHostEndpointName)
		return err
	}
	if err != nil {
		e.recorder.Warningf("setBootstrapMember", "Error getting %s/%s endpoint: %v", clustermembercontroller.EtcdEndpointNamespace, clustermembercontroller.EtcdHostEndpointName, err)
		return err
	}

	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.Hostname == "etcd-bootstrap" {
				name := address.Hostname
				peerURLs := fmt.Sprintf("https://%s.%s:2380", name, e.ClusterDomain)
				clusterMember, err := setMember(name, []string{peerURLs}, ceoapi.MemberUnknown)
				if err != nil {
					return err
				}
				e.ObservedMembers = append(e.ObservedMembers, clusterMember)
			}
		}
	}
	return nil
}

func (e *etcdObserver) setObservedEtcdFromEndpoint(bucket string) error {
	var endpointAddressList []corev1.EndpointAddress

	endpoints, err := e.listers.OpenshiftEtcdEndpointsLister.Endpoints(clustermembercontroller.EtcdEndpointNamespace).Get(clustermembercontroller.EtcdEndpointName)
	if errors.IsNotFound(err) {
		e.recorder.Warningf("setObservedFromEndpoint", "Required %s/%s endpoint not found", clustermembercontroller.EtcdEndpointNamespace, clustermembercontroller.EtcdHostEndpointName)
		return err
	}
	if err != nil {
		e.recorder.Warningf("setObservedFromEndpoint", "Error getting %s/%s endpoint: %v", clustermembercontroller.EtcdEndpointNamespace, clustermembercontroller.EtcdHostEndpointName, err)
		return err
	}
	for _, subset := range endpoints.Subsets {
		switch bucket {
		case "members":
			endpointAddressList = subset.Addresses
		case "pending":
			// probably should be a struct?
			endpointAddressList = subset.NotReadyAddresses
		}
		for _, address := range endpointAddressList {
			name := address.TargetRef.Name
			cm, err := e.listers.OpenshiftEtcdConfigMapsLister.ConfigMaps(clustermembercontroller.EtcdEndpointNamespace).Get("member-config")
			if err != nil {
				return err
			}
			scalingName, err := clustermembercontroller.GetScaleAnnotationName(cm)
			if err != nil {
				return err
			}
			pod, err := e.listers.OpenshiftEtcdPodsLister.Pods(clustermembercontroller.EtcdEndpointNamespace).Get(name)
			if err != nil {
				return err
			}

			status := ceoapi.MemberUnknown
			switch {
			case isPodCrashLoop(bucket, pod):
				status = ceoapi.MemberRemove
				break
			case isPendingReady(bucket, pod.Name, scalingName, e.listers.OpenshiftEtcdPodsLister):
				status = ceoapi.MemberReady
				break
			default:
				status = ceoapi.MemberUnknown
			}

			var peerFQDN string
			// allow testing
			if e.ClusterDomain == "operator.testing.openshift" {
				peerFQDN = "etcd-1.operator.testing.openshift"
			} else {
				peerFQDN, err = clustermembercontroller.ReverseLookupSelf("etcd-server-ssl", "tcp", e.ClusterDomain, address.IP)
				if err != nil {
					return fmt.Errorf("error looking up self: %v", err)
				}
			}

			peerURLs := fmt.Sprintf("https://%s:2380", peerFQDN)
			etcd, err := setMember(name, []string{peerURLs}, status)
			if err != nil {
				return err
			}

			switch bucket {
			case "members":
				e.HealthyMember[name] = true
				e.ObservedMembers = append(e.ObservedMembers, etcd)
			case "pending":
				e.ObservedPending = append(e.ObservedPending, etcd)
			}
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
