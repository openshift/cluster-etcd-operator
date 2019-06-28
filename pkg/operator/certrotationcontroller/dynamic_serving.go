package certrotationcontroller

import (
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
)

// DynamicServingRotation is a threadsafe struct to provide hostname methods for certrotation.ServingRotation info.
// This allows us to change the hostnames and get our certs regenerated.
type DynamicServingRotation struct {
	lock             sync.RWMutex
	hostnames        []string
	hostnamesChanged chan struct{}
}

func (r *DynamicServingRotation) setHostnames(newHostnames []string) {
	if r.isSame(newHostnames) {
		return
	}

	r.lock.Lock()
	r.hostnames = newHostnames
	r.lock.Unlock()
	r.hostnamesChanged <- struct{}{}
}

func (r *DynamicServingRotation) isSame(newHostnames []string) bool {
	r.lock.RLock()
	defer r.lock.RUnlock()
	existingSet := sets.NewString(r.hostnames...)
	newSet := sets.NewString(newHostnames...)
	return existingSet.Equal(newSet)
}

func (r *DynamicServingRotation) GetHostnames() []string {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.hostnames
}
