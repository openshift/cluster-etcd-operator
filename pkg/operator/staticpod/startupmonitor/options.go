package startupmonitor

import (
	"time"

	operatorclientv1 "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
)

// withProbeInterval probeInterval specifies a time interval at which health of the target will be assessed.
// Be mindful of not setting it too low, on each iteration, an i/o is involved
func (m *monitor) withProbeInterval(probeInterval time.Duration) *monitor {
	m.probeInterval = probeInterval
	return m
}

// withTimeout sets the readiness timeout.
func (m *monitor) withTimeout(timeout time.Duration) *monitor {
	m.timeout = timeout
	return m
}

// withTargetName specifies the name of the operand
// used to construct the final file name when reading the current and previous manifests
func (m *monitor) withTargetName(targetName string) *monitor {
	m.targetName = targetName
	return m
}

// withManifestPath points to the directory that holds the root manifests
func (m *monitor) withManifestPath(manifestsPath string) *monitor {
	m.manifestsPath = manifestsPath
	return m
}

// withRevision specifies the current revision number
func (m *monitor) withRevision(revision int) *monitor {
	m.revision = revision
	return m
}

// withTargetName specifies the name of the operand
// used to construct the final file name when reading the current and previous manifests
func (f *staticPodFallback) withTargetName(targetName string) *staticPodFallback {
	f.targetName = targetName
	return f
}

// withManifestPath points to the directory that holds the root manifests
func (f *staticPodFallback) withManifestPath(manifestsPath string) *staticPodFallback {
	f.manifestsPath = manifestsPath
	return f
}

// withStaticPodResourcesPath points to the directory that holds all files supporting the static pod manifests
func (f *staticPodFallback) withStaticPodResourcesPath(staticPodResourcesPath string) *staticPodFallback {
	f.staticPodResourcesPath = staticPodResourcesPath
	return f
}

// withRevision specifies the current revision number
func (f *staticPodFallback) withRevision(revision int) *staticPodFallback {
	f.revision = revision
	return f
}

func (f *staticPodFallback) withOperatorClient(operatorClient operatorclientv1.KubeAPIServerInterface) *staticPodFallback {
	f.operatorClient = operatorClient
	return f
}

func (f *staticPodFallback) withNodeName(nodeName string) *staticPodFallback {
	f.nodeName = nodeName
	return f
}
