package status

import (
	"context"
	"os"
	"sync"

	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/openshift/library-go/pkg/operator/events"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type versionGetter struct {
	lock                 sync.Mutex
	versions             map[string]string
	notificationChannels []chan struct{}
}

const (
	operandImageEnvVarName         = "IMAGE"
	operandImageVersionEnvVarName  = "OPERAND_IMAGE_VERSION"
	operatorImageVersionEnvVarName = "OPERATOR_IMAGE_VERSION"
)

func NewVersionGetter() VersionGetter {
	return &versionGetter{
		versions: map[string]string{},
	}
}

func (v *versionGetter) SetVersion(operandName, version string) {
	v.lock.Lock()
	defer v.lock.Unlock()

	v.versions[operandName] = version
	v.notifyChannelsLocked()
}

func (v *versionGetter) UnsetVersion(operandName string) {
	v.lock.Lock()
	defer v.lock.Unlock()

	if _, exists := v.versions[operandName]; !exists {
		return
	}

	delete(v.versions, operandName)
	v.notifyChannelsLocked()
}

func (v *versionGetter) GetVersions() map[string]string {
	v.lock.Lock()
	defer v.lock.Unlock()

	ret := map[string]string{}
	for k, v := range v.versions {
		ret[k] = v
	}
	return ret
}

func (v *versionGetter) VersionChangedChannel() <-chan struct{} {
	v.lock.Lock()
	defer v.lock.Unlock()

	channel := make(chan struct{}, 50)
	v.notificationChannels = append(v.notificationChannels, channel)
	return channel
}

// notifyChannelsLocked must be called under a locked v.lock
func (v *versionGetter) notifyChannelsLocked() {
	for i := range v.notificationChannels {
		ch := v.notificationChannels[i]
		// don't let a slow consumer block the rest
		go func() {
			ch <- struct{}{}
		}()
	}
}

func ImageForOperandFromEnv() string {
	return os.Getenv(operandImageEnvVarName)
}

func VersionForOperandFromEnv() string {
	return os.Getenv(operandImageVersionEnvVarName)
}

func VersionForOperatorFromEnv() string {
	return os.Getenv(operatorImageVersionEnvVarName)
}

func VersionForOperand(namespace, imagePullSpec string, configMapGetter corev1client.ConfigMapsGetter, eventRecorder events.Recorder) string {
	versionMap := map[string]string{}
	versionMapping, err := configMapGetter.ConfigMaps(namespace).Get(context.TODO(), "version-mapping", metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		eventRecorder.Warningf("VersionMappingFailure", "unable to get version mapping: %v", err)
		return ""
	}
	if versionMapping != nil {
		for version, image := range versionMapping.Data {
			versionMap[image] = version
		}
	}

	// we have the actual daemonset and we need the pull spec
	operandVersion := versionMap[imagePullSpec]
	return operandVersion
}
