package installerpod

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/utils/clock"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/blang/semver/v4"
	"github.com/davecgh/go-spew/spew"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/openshift/library-go/pkg/config/client"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/resource/retry"
	"github.com/openshift/library-go/pkg/operator/staticpod"
	"github.com/openshift/library-go/pkg/operator/staticpod/internal"
	"github.com/openshift/library-go/pkg/operator/staticpod/internal/flock"
)

type InstallOptions struct {
	// TODO replace with genericclioptions
	KubeConfig string
	KubeClient kubernetes.Interface

	Revision  string
	NodeName  string
	Namespace string
	Clock     clock.PassiveClock

	PodConfigMapNamePrefix        string
	SecretNamePrefixes            []string
	OptionalSecretNamePrefixes    []string
	ConfigMapNamePrefixes         []string
	OptionalConfigMapNamePrefixes []string

	CertSecretNames                   []string
	OptionalCertSecretNamePrefixes    []string
	CertConfigMapNamePrefixes         []string
	OptionalCertConfigMapNamePrefixes []string

	CertDir        string
	ResourceDir    string
	PodManifestDir string

	Timeout time.Duration

	// StaticPodManifestsLockFile used to coordinate work between multiple processes when writing static pod manifests
	StaticPodManifestsLockFile string

	PodMutationFns []PodMutationFunc

	KubeletVersion string
}

// PodMutationFunc is a function that has a chance at changing the pod before it is created
type PodMutationFunc func(pod *corev1.Pod) error

func NewInstallOptions() *InstallOptions {
	return &InstallOptions{
		Clock: clock.RealClock{},
	}
}

func (o *InstallOptions) WithPodMutationFn(podMutationFn PodMutationFunc) *InstallOptions {
	o.PodMutationFns = append(o.PodMutationFns, podMutationFn)
	return o
}

func NewInstaller(ctx context.Context) *cobra.Command {
	o := NewInstallOptions()

	cmd := &cobra.Command{
		Use:   "installer",
		Short: "Install static pod and related resources",
		Run: func(cmd *cobra.Command, args []string) {
			klog.V(1).Info(cmd.Flags())
			klog.V(1).Info(spew.Sdump(o))

			if err := o.Complete(); err != nil {
				klog.Exit(err)
			}
			if err := o.Validate(); err != nil {
				klog.Exit(err)
			}

			signalHandlingCtx := setupSignalContext(ctx)
			ctx, cancel := context.WithTimeout(signalHandlingCtx, o.Timeout)
			defer cancel()
			if err := o.Run(ctx); err != nil {
				klog.Exit(err)
			}
		},
	}

	o.AddFlags(cmd.Flags())

	return cmd
}

// setupSignalContext registers for SIGTERM and SIGINT and returns a context
// that will be cancelled once a signal is received.
func setupSignalContext(baseCtx context.Context) context.Context {
	shutdownCtx, cancel := context.WithCancel(baseCtx)
	shutdownHandler := server.SetupSignalHandler()
	go func() {
		defer cancel()
		<-shutdownHandler
		klog.Infof("Received SIGTERM or SIGINT signal, shutting down the process.")
	}()
	return shutdownCtx
}

func (o *InstallOptions) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.KubeConfig, "kubeconfig", o.KubeConfig, "kubeconfig file or empty")
	fs.StringVar(&o.Revision, "revision", o.Revision, "identifier for this particular installation instance.  For example, a counter or a hash")
	fs.StringVar(&o.Namespace, "namespace", o.Namespace, "namespace to retrieve all resources from and create the static pod in")
	fs.StringVar(&o.PodConfigMapNamePrefix, "pod", o.PodConfigMapNamePrefix, "name of configmap that contains the pod to be created")
	fs.StringSliceVar(&o.SecretNamePrefixes, "secrets", o.SecretNamePrefixes, "list of secret names to be included")
	fs.StringSliceVar(&o.ConfigMapNamePrefixes, "configmaps", o.ConfigMapNamePrefixes, "list of configmaps to be included")
	fs.StringSliceVar(&o.OptionalSecretNamePrefixes, "optional-secrets", o.OptionalSecretNamePrefixes, "list of optional secret names to be included")
	fs.StringSliceVar(&o.OptionalConfigMapNamePrefixes, "optional-configmaps", o.OptionalConfigMapNamePrefixes, "list of optional configmaps to be included")
	fs.StringVar(&o.ResourceDir, "resource-dir", o.ResourceDir, "directory for all files supporting the static pod manifest")
	fs.StringVar(&o.PodManifestDir, "pod-manifest-dir", o.PodManifestDir, "directory for the static pod manifest")
	fs.DurationVar(&o.Timeout, "timeout-duration", 120*time.Second, "maximum time in seconds to wait for the copying to complete (default: 2m)")
	fs.StringVar(&o.StaticPodManifestsLockFile, "pod-manifests-lock-file", o.StaticPodManifestsLockFile, "path to a file that will be used to coordinate writing static pod manifests between multiple processes")

	fs.StringSliceVar(&o.CertSecretNames, "cert-secrets", o.CertSecretNames, "list of secret names to be included")
	fs.StringSliceVar(&o.CertConfigMapNamePrefixes, "cert-configmaps", o.CertConfigMapNamePrefixes, "list of configmaps to be included")
	fs.StringSliceVar(&o.OptionalCertSecretNamePrefixes, "optional-cert-secrets", o.OptionalCertSecretNamePrefixes, "list of optional secret names to be included")
	fs.StringSliceVar(&o.OptionalCertConfigMapNamePrefixes, "optional-cert-configmaps", o.OptionalCertConfigMapNamePrefixes, "list of optional configmaps to be included")
	fs.StringVar(&o.CertDir, "cert-dir", o.CertDir, "directory for all certs")
}

func (o *InstallOptions) Complete() error {
	clientConfig, err := client.GetKubeConfigOrInClusterConfig(o.KubeConfig, nil)
	if err != nil {
		return err
	}

	// Use protobuf to fetch configmaps and secrets and create pods.
	protoConfig := rest.CopyConfig(clientConfig)
	protoConfig.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	protoConfig.ContentType = "application/vnd.kubernetes.protobuf"
	protoConfig.Timeout = 14 * time.Second

	o.KubeClient, err = kubernetes.NewForConfig(protoConfig)
	if err != nil {
		return err
	}

	// set via downward API
	o.NodeName = os.Getenv("NODE_NAME")

	return nil
}

func (o *InstallOptions) Validate() error {
	if len(o.Revision) == 0 {
		return fmt.Errorf("--revision is required")
	}
	if len(o.NodeName) == 0 {
		return fmt.Errorf("env var NODE_NAME is required")
	}
	if len(o.Namespace) == 0 {
		return fmt.Errorf("--namespace is required")
	}
	if len(o.PodConfigMapNamePrefix) == 0 {
		return fmt.Errorf("--pod is required")
	}
	if len(o.ConfigMapNamePrefixes) == 0 {
		return fmt.Errorf("--configmaps is required")
	}
	if o.Timeout == 0 {
		return fmt.Errorf("--timeout-duration cannot be 0")
	}

	if o.KubeClient == nil {
		return fmt.Errorf("missing client")
	}

	return nil
}

func (o *InstallOptions) nameFor(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, o.Revision)
}

func (o *InstallOptions) prefixFor(name string) string {
	return name[0 : len(name)-len(fmt.Sprintf("-%s", o.Revision))]
}

func (o *InstallOptions) kubeletVersion(ctx context.Context) (string, error) {
	node, err := o.KubeClient.CoreV1().Nodes().Get(ctx, o.NodeName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return node.Status.NodeInfo.KubeletVersion, nil
}

func (o *InstallOptions) copySecretsAndConfigMaps(ctx context.Context, resourceDir string,
	secretNames, optionalSecretNames, configNames, optionalConfigNames sets.Set[string], prefixed bool) error {
	klog.Infof("Creating target resource directory %q ...", resourceDir)
	if err := os.MkdirAll(resourceDir, 0755); err != nil && !os.IsExist(err) {
		return err
	}

	// Gather secrets. If we get API server error, retry getting until we hit the timeout.
	// Retrying will prevent temporary API server blips or networking issues.
	// We return when all "required" secrets are gathered, optional secrets are not checked.
	klog.Infof("Getting secrets ...")
	secrets := []*corev1.Secret{}
	for _, name := range append(sets.List(secretNames), sets.List(optionalSecretNames)...) {
		secret, err := o.getSecretWithRetry(ctx, name, optionalSecretNames.Has(name))
		if err != nil {
			return err
		}
		// secret is nil means the secret was optional and we failed to get it.
		if secret != nil {
			secrets = append(secrets, o.substituteSecret(secret))
		}
	}

	klog.Infof("Getting config maps ...")
	configs := []*corev1.ConfigMap{}
	for _, name := range append(sets.List(configNames), sets.List(optionalConfigNames)...) {
		config, err := o.getConfigMapWithRetry(ctx, name, optionalConfigNames.Has(name))
		if err != nil {
			return err
		}
		// config is nil means the config was optional and we failed to get it.
		if config != nil {
			configs = append(configs, o.substituteConfigMap(config))
		}
	}

	for _, secret := range secrets {
		secretBaseName := secret.Name
		if prefixed {
			secretBaseName = o.prefixFor(secret.Name)
		}
		contentDir := path.Join(resourceDir, "secrets", secretBaseName)
		klog.Infof("Creating directory %q ...", contentDir)
		if err := os.MkdirAll(contentDir, 0755); err != nil {
			return err
		}
		for filename, content := range secret.Data {
			if err := writeSecret(content, path.Join(contentDir, filename)); err != nil {
				return err
			}
		}
	}
	for _, configmap := range configs {
		configMapBaseName := configmap.Name
		if prefixed {
			configMapBaseName = o.prefixFor(configmap.Name)
		}
		contentDir := path.Join(resourceDir, "configmaps", configMapBaseName)
		klog.Infof("Creating directory %q ...", contentDir)
		if err := os.MkdirAll(contentDir, 0755); err != nil {
			return err
		}
		for filename, content := range configmap.Data {
			if err := writeConfig([]byte(content), path.Join(contentDir, filename)); err != nil {
				return err
			}
		}
	}

	return nil
}

func (o *InstallOptions) copyContent(ctx context.Context) error {
	resourceDir := path.Join(o.ResourceDir, o.nameFor(o.PodConfigMapNamePrefix))
	klog.Infof("Creating target resource directory %q ...", resourceDir)
	if err := os.MkdirAll(resourceDir, 0755); err != nil && !os.IsExist(err) {
		return err
	}

	secretPrefixes := sets.New[string]()
	optionalSecretPrefixes := sets.New[string]()
	configPrefixes := sets.New[string]()
	optionalConfigPrefixes := sets.New[string]()
	for _, prefix := range o.SecretNamePrefixes {
		secretPrefixes.Insert(o.nameFor(prefix))
	}
	for _, prefix := range o.OptionalSecretNamePrefixes {
		optionalSecretPrefixes.Insert(o.nameFor(prefix))
	}
	for _, prefix := range o.ConfigMapNamePrefixes {
		configPrefixes.Insert(o.nameFor(prefix))
	}
	for _, prefix := range o.OptionalConfigMapNamePrefixes {
		optionalConfigPrefixes.Insert(o.nameFor(prefix))
	}
	if err := o.copySecretsAndConfigMaps(ctx, resourceDir, secretPrefixes, optionalSecretPrefixes, configPrefixes, optionalConfigPrefixes, true); err != nil {
		return err
	}

	// Copy the current state of the certs as we see them.  This primes us once and allows a kube-apiserver to start once
	if len(o.CertDir) > 0 {
		if err := o.copySecretsAndConfigMaps(ctx, o.CertDir,
			sets.New(o.CertSecretNames...),
			sets.New(o.OptionalCertSecretNamePrefixes...),
			sets.New(o.CertConfigMapNamePrefixes...),
			sets.New(o.OptionalCertConfigMapNamePrefixes...),
			false,
		); err != nil {
			return err
		}
	}

	// Gather the config map that holds pods to be installed
	var podsConfigMap *corev1.ConfigMap

	err := retry.RetryOnConnectionErrors(ctx, func(ctx context.Context) (bool, error) {
		klog.Infof("Getting pod configmaps/%s -n %s", o.nameFor(o.PodConfigMapNamePrefix), o.Namespace)
		podConfigMap, err := o.KubeClient.CoreV1().ConfigMaps(o.Namespace).Get(ctx, o.nameFor(o.PodConfigMapNamePrefix), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if _, exists := podConfigMap.Data["pod.yaml"]; !exists {
			return true, fmt.Errorf("required 'pod.yaml' key does not exist in configmap")
		}
		podsConfigMap = o.substituteConfigMap(podConfigMap)
		return true, nil
	})
	if err != nil {
		return err
	}

	// at this point we know that the required key is present in the config map, just make sure the manifest dir actually exists
	klog.Infof("Creating directory for static pod manifest %q ...", o.PodManifestDir)
	if err := os.MkdirAll(o.PodManifestDir, 0755); err != nil {
		return err
	}

	// check to see if we need to acquire a file based lock to coordinate work.
	// since writing to disk is fast and we need to write at most 2 files it is okay to hold a lock here
	// note that in case of unplanned disaster the Linux kernel is going to release the lock when the process exits
	if len(o.StaticPodManifestsLockFile) > 0 {
		installerLock := flock.New(o.StaticPodManifestsLockFile)
		klog.Infof("acquiring an exclusive lock on a %s", o.StaticPodManifestsLockFile)
		if err := installerLock.Lock(ctx); err != nil {
			return fmt.Errorf("failed to acquire an exclusive lock on %s, due to %v", o.StaticPodManifestsLockFile, err)
		}
		defer installerLock.Unlock()
	}

	// then write the required pod and all optional
	// the key must be pod.yaml or has a -pod.yaml suffix to be considered
	for rawPodKey, rawPod := range podsConfigMap.Data {
		var manifestFileName = rawPodKey
		if manifestFileName == "pod.yaml" {
			// TODO: update kas-o to update the key to a fully qualified name
			manifestFileName = o.PodConfigMapNamePrefix + ".yaml"
		} else if !strings.HasSuffix(manifestFileName, "-pod.yaml") {
			continue
		}

		klog.Infof("Writing a pod under %q key \n%s", manifestFileName, rawPod)
		err := o.writePod([]byte(rawPod), manifestFileName, resourceDir)
		if err != nil {
			return err
		}
	}
	return nil
}

func (o *InstallOptions) substituteConfigMap(obj *corev1.ConfigMap) *corev1.ConfigMap {
	ret := obj.DeepCopy()
	for k, oldContent := range obj.Data {
		newContent := strings.ReplaceAll(oldContent, "REVISION", o.Revision)
		newContent = strings.ReplaceAll(newContent, "NODE_NAME", o.NodeName)
		newContent = strings.ReplaceAll(newContent, "NODE_ENVVAR_NAME", strings.ReplaceAll(strings.ReplaceAll(o.NodeName, "-", "_"), ".", "_"))
		ret.Data[k] = newContent
	}
	return ret
}

func (o *InstallOptions) substituteSecret(obj *corev1.Secret) *corev1.Secret {
	ret := obj.DeepCopy()
	for k, oldContent := range obj.Data {
		newContent := strings.ReplaceAll(string(oldContent), "REVISION", o.Revision)
		newContent = strings.ReplaceAll(newContent, "NODE_NAME", o.NodeName)
		newContent = strings.ReplaceAll(newContent, "NODE_ENVVAR_NAME", strings.ReplaceAll(strings.ReplaceAll(o.NodeName, "-", "_"), ".", "_"))
		ret.Data[k] = []byte(newContent)
	}
	return ret
}

func (o *InstallOptions) Run(ctx context.Context) error {
	var eventTarget *corev1.ObjectReference

	klog.Infof("Getting controller reference for node %s", o.NodeName)
	err := retry.RetryOnConnectionErrors(ctx, func(context.Context) (bool, error) {
		var clientErr error
		eventTarget, clientErr = events.GetControllerReferenceForCurrentPod(ctx, o.KubeClient, o.Namespace, nil)
		if clientErr != nil {
			return false, clientErr
		}
		return true, nil
	})
	if err != nil {
		klog.Warningf("unable to get owner reference (falling back to namespace): %v", err)
	}

	klog.Infof("Waiting for installer revisions to settle for node %s", o.NodeName)
	if err := o.waitForOtherInstallerRevisionsToSettle(ctx); err != nil {
		return err
	}

	klog.Infof("Querying kubelet version for node %s", o.NodeName)
	err = retry.RetryOnConnectionErrors(ctx, func(context.Context) (bool, error) {
		version, err := o.kubeletVersion(ctx)
		if err != nil {
			return false, err
		}
		// trim 'v' from 'v1.22.1' because semver parsing don't like it
		o.KubeletVersion = strings.TrimPrefix(version, "v")
		return true, nil
	})
	if err != nil || len(o.KubeletVersion) == 0 {
		klog.Warningf("unable to get kubelet version for node %q: %v", o.NodeName, err)
	} else {
		klog.Infof("Got kubelet version %s on target node %s", o.KubeletVersion, o.NodeName)
	}

	recorder := events.NewRecorder(o.KubeClient.CoreV1().Events(o.Namespace), "static-pod-installer", eventTarget, o.Clock)
	if err := o.copyContent(ctx); err != nil {
		recorder.Warningf("StaticPodInstallerFailed", "Installing revision %s: %v", o.Revision, err)
		return fmt.Errorf("failed to copy: %v", err)
	}

	recorder.Eventf("StaticPodInstallerCompleted", "Successfully installed revision %s", o.Revision)
	return nil
}

func (o *InstallOptions) waitForOtherInstallerRevisionsToSettle(ctx context.Context) error {
	currRevision64, err := strconv.ParseInt(o.Revision, 10, 32)
	if err != nil {
		return fmt.Errorf("bad local revision %v: %w", o.Revision, err)
	}
	currRevision := int(currRevision64)

	// at this point we have a list of all the installer pods present on this node we need to check
	// 1. is there a revision later than us present right now.  If yes, exit.
	// 2. is there an installer pod present that has not finished running, wait until it is finished.
	err = wait.PollImmediateWithContext(ctx, 10*time.Second, 60*time.Second, func(ctx context.Context) (done bool, err error) {
		installerPods, err := o.getInstallerPodsOnThisNode(ctx)
		if err != nil {
			klog.Warningf("Error getting installer pods on current node %s: %v", o.NodeName, err)
			return false, nil
		}
		if len(installerPods) == 0 {
			return false, fmt.Errorf("no installer pods found")
		}
		latestRevision, err := internal.GetRevisionOfPod(installerPods[len(installerPods)-1])
		if err != nil {
			return false, err
		}
		// if there is an installer pod for a newer revision, this installer pod should quit.
		if latestRevision > currRevision {
			return false, fmt.Errorf("more recent revision present on node: thisRevision=%v, moreRecentRevision=%v", currRevision, latestRevision)
		}

		// if any container is not terminated, we need to wait.  There should only ever be a single installer pod
		// that could be run
		for _, pod := range installerPods {
			// skip this revision
			podRevision, err := internal.GetRevisionOfPod(pod)
			if err != nil {
				return false, err
			}

			// skip our own installer pod.
			if podRevision == currRevision {
				continue
			}
			// wait until we have at least one container status to check
			if len(pod.Status.ContainerStatuses) == 0 {
				klog.Infof("Pod container statuses for node %s are empty, waiting", o.NodeName)
				return false, nil
			}
			for _, container := range pod.Status.ContainerStatuses {
				// if the container isn't terminated, we need this installer pod to wait until it is.
				if container.State.Terminated == nil {
					klog.Infof("Pod container: %s state for node %s is not terminated, waiting", container.Name, o.NodeName)
					return false, nil
				}
			}
		}

		return true, nil
	})
	if err != nil {
		return err
	}

	klog.Infof("Waiting additional period after revisions have settled for node %s", o.NodeName)
	// once there are no other running revisions, wait Xs.
	// In an extreme case, this can be grace period seconds+1.  Trying 30s to start. Etcd has been the worst off since
	// it requires 2 out 3 to be functioning.
	time.Sleep(30 * time.Second)

	klog.Infof("Getting installer pods for node %s", o.NodeName)
	installerPods, err := o.getInstallerPodsOnThisNode(ctx)
	if err != nil {
		return err
	}
	// if there are no installer pods, it means this pod was removed somehow.  no action should be taken.
	if len(installerPods) == 0 {
		return fmt.Errorf("no installer pods found")
	}
	latestRevision, err := internal.GetRevisionOfPod(installerPods[len(installerPods)-1])
	if err != nil {
		return err
	}
	// if there is an installer pod for a newer revision, this installer pod should quit.
	if latestRevision > currRevision {
		return fmt.Errorf("more recent revision present on node: thisRevision=%v, moreRecentRevision=%v", currRevision, latestRevision)
	}

	klog.Infof("Latest installer revision for node %s is: %v", o.NodeName, latestRevision)

	return nil
}

func (o *InstallOptions) getInstallerPodsOnThisNode(ctx context.Context) ([]*corev1.Pod, error) {
	podList, err := o.KubeClient.CoreV1().Pods(o.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=installer"})
	if err != nil {
		return nil, err
	}
	installerPodsOnThisNode := []*corev1.Pod{}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if !strings.HasPrefix(pod.Name, "installer-") {
			continue
		}
		if pod.Spec.NodeName != o.NodeName {
			continue
		}
		installerPodsOnThisNode = append(installerPodsOnThisNode, pod)
	}
	sort.Sort(internal.ByRevision(installerPodsOnThisNode))

	return installerPodsOnThisNode, nil
}

func (o *InstallOptions) installerPodNeedUUID() bool {
	kubeletVersion, err := semver.Make(o.KubeletVersion)
	if err != nil {
		klog.Warningf("Failed to parse kubelet version %q to semver: %v (we will not generate pod UID)", o.KubeletVersion, err)
		return false
	}
	// if we run kubelet older than 4.9, installer pods require UID generation
	// Switching this to 1.23 because there was a fix backported late to 1.22: https://github.com/kubernetes/kubernetes/pull/106394
	// Once we prove this out in 1.23 after the kube bump, we can consider relaxing the constraint to 1.22.Z, but TRT
	// suspects that this is failing installs due to
	//  hyperkube[1371]: I1202 08:50:47.935427    1371 status_manager.go:611] "Pod was deleted and then recreated, skipping status update" pod="openshift-kube-scheduler/openshift-kube-scheduler-ip-10-0-129-204.us-east-2.compute.internal" oldPodUID=f11e314508a00e4ea9bf37d76b82b162 podUID=cce59ead72804895d31dba4208b395aa
	// in kubelet logs
	return kubeletVersion.LT(semver.MustParse("1.23.0"))
}

func (o *InstallOptions) writePod(rawPodBytes []byte, manifestFileName, resourceDir string) error {
	// the kubelet has a bug that prevents graceful termination from working on static pods with the same name, filename
	// and uuid.  By setting the pod UID we can work around the kubelet bug and get our graceful termination honored.
	// Per the node team, this is hard to fix in the kubelet, though it will affect all static pods.
	pod, err := resourceread.ReadPodV1(rawPodBytes)
	if err != nil {
		return err
	}

	//  If the kubelet version is >=4.9 (1.22), the installer pod UID does not need to be set in the file.
	if o.installerPodNeedUUID() {
		pod.UID = uuid.NewUUID()
	} else {
		pod.UID = ""
	}

	for _, fn := range o.PodMutationFns {
		klog.V(2).Infof("Customizing static pod ...")
		pod = pod.DeepCopy()
		if err := fn(pod); err != nil {
			return err
		}
	}
	finalPodBytes := resourceread.WritePodV1OrDie(pod)

	// Write secrets, config maps and pod to disk
	// This does not need timeout, instead we should fail hard when we are not able to write.
	klog.Infof("Writing pod manifest %q ...", path.Join(resourceDir, manifestFileName))
	if err := os.WriteFile(path.Join(resourceDir, manifestFileName), []byte(finalPodBytes), 0600); err != nil {
		return err
	}

	// remove the existing file to ensure kubelet gets "create" event from inotify watchers
	if err := os.Remove(path.Join(o.PodManifestDir, manifestFileName)); err == nil {
		klog.Infof("Removed existing static pod manifest %q ...", path.Join(o.PodManifestDir, manifestFileName))
	} else if !os.IsNotExist(err) {
		return err
	}
	klog.Infof("Writing static pod manifest %q ...\n%s", path.Join(o.PodManifestDir, manifestFileName), finalPodBytes)
	if err := os.WriteFile(path.Join(o.PodManifestDir, manifestFileName), []byte(finalPodBytes), 0600); err != nil {
		return err
	}
	return nil
}

func writeConfig(content []byte, fullFilename string) error {
	klog.Infof("Writing config file %q ...", fullFilename)

	filePerms := os.FileMode(0600)
	if strings.HasSuffix(fullFilename, ".sh") {
		filePerms = 0755
	}
	return staticpod.WriteFileAtomic(content, filePerms, fullFilename)
}

func writeSecret(content []byte, fullFilename string) error {
	klog.Infof("Writing secret manifest %q ...", fullFilename)

	filePerms := os.FileMode(0600)
	if strings.HasSuffix(fullFilename, ".sh") {
		filePerms = 0700
	}
	return staticpod.WriteFileAtomic(content, filePerms, fullFilename)
}
