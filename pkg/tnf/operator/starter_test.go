package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/api/features"
	operatorv1 "github.com/openshift/api/operator/v1"
	configfake "github.com/openshift/client-go/config/clientset/versioned/fake"
	configv1informersfactory "github.com/openshift/client-go/config/informers/externalversions"
	applyoperatorv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"
	operatorfake "github.com/openshift/client-go/operator/clientset/versioned/fake"
	operatorinformers "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/bindata"
	"github.com/openshift/cluster-etcd-operator/pkg/backuphelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdenvvar"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/jobs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pacemaker"
	"github.com/openshift/library-go/pkg/apiserver/jsonpatch"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	corev1informers "k8s.io/client-go/informers/core/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"
	"sigs.k8s.io/yaml"
)

func TestTNFObservabilityFeatureGate(t *testing.T) {
	gate := features.FeatureGateTNFObservability
	for _, tt := range []struct {
		name     string
		enabled  []configv1.FeatureGateName
		disabled []configv1.FeatureGateName
		want     bool
	}{
		{name: "older payload does not know gate"},
		{name: "legacy alert-agent gate does not enable observability", enabled: []configv1.FeatureGateName{"PacemakerAlertAgentStatus"}},
		{name: "disabled", disabled: []configv1.FeatureGateName{gate}},
		{name: "enabled", enabled: []configv1.FeatureGateName{gate}, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			enabled := pacemaker.AlertAgentStatusEnabled(featuregates.NewFeatureGate(tt.enabled, tt.disabled))
			require.Equal(t, tt.want, enabled)

			var crd apiextensionsv1.CustomResourceDefinition
			require.NoError(t, yaml.Unmarshal(bindata.MustAsset(pacemakerCRDAsset(enabled)), &crd))
			require.Equal(t, "pacemakerclusters.etcd.openshift.io", crd.Name)
			require.Len(t, crd.Spec.Versions, 1)
			require.Equal(t, "v1", crd.Spec.Versions[0].Name)
			node := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties["nodes"].Items.Schema
			_, hasAgents := node.Properties["alertAgents"]
			require.Equal(t, tt.want, hasAgents, "schema must match the collector's gate")
			retainsAgents := false
			for _, validation := range node.XValidations {
				if validation.Rule == "!has(oldSelf.alertAgents) || has(self.alertAgents)" {
					retainsAgents = true
				}
			}
			require.Equal(t, tt.want, retainsAgents, "retention validation must also be gated")
		})
	}
}

func TestPacemakerCRDMatchesAPIFeatureSets(t *testing.T) {
	for _, profile := range []struct {
		name string
		api  features.ClusterProfileName
	}{
		{name: "SelfManagedHA", api: features.SelfManaged},
		{name: "Hypershift", api: features.Hypershift},
	} {
		for _, featureSet := range []configv1.FeatureSet{configv1.Default, configv1.OKD, configv1.TechPreviewNoUpgrade, configv1.DevPreviewNoUpgrade} {
			name := string(featureSet)
			if name == "" {
				name = "Default"
			}
			t.Run(profile.name+"/"+name, func(t *testing.T) {
				resolved := features.FeatureSets(5, profile.api, featureSet)
				var enabled, disabled []configv1.FeatureGateName
				for _, gate := range resolved.Enabled {
					enabled = append(enabled, gate.FeatureGateAttributes.Name)
				}
				for _, gate := range resolved.Disabled {
					disabled = append(disabled, gate.FeatureGateAttributes.Name)
				}
				observability := pacemaker.AlertAgentStatusEnabled(featuregates.NewFeatureGate(enabled, disabled))
				want := profile.api == features.SelfManaged && (featureSet == configv1.TechPreviewNoUpgrade || featureSet == configv1.DevPreviewNoUpgrade)
				require.Equal(t, want, observability)

				source := filepath.Join("..", "..", "..", "vendor", "github.com", "openshift", "api", "etcd", "v1", "zz_generated.crd-manifests",
					fmt.Sprintf("0000_25_etcd_01_pacemakerclusters-%s-%s.crd.yaml", profile.name, name))
				data, err := os.ReadFile(source)
				require.NoError(t, err)
				var upstream, embedded apiextensionsv1.CustomResourceDefinition
				require.NoError(t, yaml.Unmarshal(data, &upstream))
				require.NoError(t, yaml.Unmarshal(bindata.MustAsset(pacemakerCRDAsset(observability)), &embedded))
				require.Equal(t, upstream.Spec, embedded.Spec, "selected schema must match the API profile and feature set")
			})
		}
	}
}

func TestPacemakerCRDReadyDuringUpgrade(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var crd apiextensionsv1.CustomResourceDefinition
		require.NoError(t, yaml.Unmarshal(bindata.MustAsset(pacemakerCRDAsset(enabled)), &crd))
		require.False(t, pacemakerCRDReady(&crd, enabled), "schema must be established first")
		crd.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{
			Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue,
		}}
		require.True(t, pacemakerCRDReady(&crd, enabled))
		require.False(t, pacemakerCRDReady(&crd, !enabled), "old established schema must not start collectors for the new gate state")
		crd.Spec.Versions[0].Served = false
		require.False(t, pacemakerCRDReady(&crd, enabled), "v1 must be served")
		crd.Spec.Versions[0].Served = true
		crd.Spec.Versions[0].Schema = nil
		require.False(t, pacemakerCRDReady(&crd, enabled), "missing schema must wait rather than panic")
	}
}

type crdRoundTripper func(*http.Request) (*http.Response, error)

func (f crdRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPacemakerCRDReconcilesFeatureGateChanges(t *testing.T) {
	for _, initiallyEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("initiallyEnabled=%t", initiallyEnabled), func(t *testing.T) {
			ctx := context.Background()
			var stored *apiextensionsv1.CustomResourceDefinition
			var writes []string
			client, err := apiextensionsclient.NewForConfig(&rest.Config{
				Host:          "https://crd-upgrade-test.invalid",
				ContentConfig: rest.ContentConfig{ContentType: "application/json"},
				Transport: crdRoundTripper(func(req *http.Request) (*http.Response, error) {
					code := http.StatusOK
					var body any
					switch req.Method {
					case http.MethodGet:
						if stored == nil {
							code = http.StatusNotFound
							body = metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound}
						} else {
							body = stored
						}
					case http.MethodPost, http.MethodPut:
						require.NoError(t, json.NewDecoder(req.Body).Decode(&stored))
						writes = append(writes, req.Method)
						body = stored
					default:
						t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
					}
					data, err := json.Marshal(body)
					require.NoError(t, err)
					return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
				}),
			})
			require.NoError(t, err)
			holder := (&resourceapply.ClientHolder{}).WithAPIExtensionsClient(client)
			recorder := events.NewInMemoryRecorder("test", clock.RealClock{})
			// Exercise the same asset application used by the static-resource controller:
			// install, change the gate on upgrade/restart, then change it back.
			for _, enabled := range []bool{initiallyEnabled, !initiallyEnabled, initiallyEnabled} {
				results := resourceapply.ApplyDirectly(ctx, holder, recorder, resourceapply.NewResourceCache(), bindata.Asset, pacemakerCRDAsset(enabled))
				require.Len(t, results, 1)
				require.NoError(t, results[0].Error)
				require.True(t, results[0].Changed)
				actual, err := client.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, "pacemakerclusters.etcd.openshift.io", metav1.GetOptions{})
				require.NoError(t, err)
				var expected apiextensionsv1.CustomResourceDefinition
				require.NoError(t, yaml.Unmarshal(bindata.MustAsset(pacemakerCRDAsset(enabled)), &expected))
				require.Equal(t, expected.Spec.Versions, actual.Spec.Versions, "replace the schema, including alert-agent fields and retention validation")
				actual.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}
				require.True(t, pacemakerCRDReady(actual, enabled))
				require.False(t, pacemakerCRDReady(actual, !enabled))
			}
			require.Equal(t, []string{http.MethodPost, http.MethodPut, http.MethodPut}, writes, "existing CRDs must be updated in place")
		})
	}
}

func TestPacemakerLifecycleManagerStartupCondition(t *testing.T) {
	client := v1helpers.NewFakeStaticPodOperatorClient(&operatorv1.StaticPodOperatorSpec{}, &operatorv1.StaticPodOperatorStatus{}, nil, nil)
	ctx := context.Background()

	require.NoError(t, setPacemakerLifecycleManagerStartupCondition(ctx, client, errors.New("informer setup failed")))
	_, status, _, err := client.GetStaticPodOperatorState()
	require.NoError(t, err)
	condition := v1helpers.FindOperatorCondition(status.Conditions, conditionTypePacemakerLifecycleManagerDegraded)
	require.NotNil(t, condition)
	require.Equal(t, operatorv1.ConditionTrue, condition.Status)
	require.Contains(t, condition.Message, "informer setup failed")

	require.NoError(t, setPacemakerLifecycleManagerStartupCondition(ctx, client, nil))
	_, status, _, err = client.GetStaticPodOperatorState()
	require.NoError(t, err)
	condition = v1helpers.FindOperatorCondition(status.Conditions, conditionTypePacemakerLifecycleManagerDegraded)
	require.NotNil(t, condition)
	require.Equal(t, operatorv1.ConditionFalse, condition.Status)
}

// lateJobControllerSyncTimeout bounds how long the regression test below waits for a TNF job
// controller to finish its cache sync. With fake clients this takes milliseconds; production
// controllers instead wait ten minutes and then exit the operator process.
const lateJobControllerSyncTimeout = 10 * time.Second

// TestHandleDualReplicaClustersRegistersJobInformerBeforeFactoryStart covers TNF job controllers
// that never finish their cache sync. RunOperator calls HandleDualReplicaClusters and then starts
// the shared namespace informer factory exactly once. The TNF job controllers are created by the
// asynchronous Pacemaker lifecycle goroutine and can be created after that Start, which does not
// run informers first requested afterwards. The openshift-etcd Jobs informer must therefore be
// registered on the synchronous TNF startup path, independent of the AutomatedEtcdBackup feature
// gate (whose controllers also register it) and of TNFObservability.
//
// The test calls the real HandleDualReplicaClusters with the real namespace informer factory and
// list/watch-backed caches over fake clients, follows the RunOperator start order, and only then
// builds a TNF job controller the way jobs.RunNodeJobController and jobs.RunClusterJobController
// do, which is the schedule that fails without the registration. The PacemakerCluster CRD is
// never reported as established, so the production lifecycle goroutine cannot request the Jobs
// informer before the factory is started.
func TestHandleDualReplicaClustersRegistersJobInformerBeforeFactoryStart(t *testing.T) {
	for _, tt := range []struct {
		name                string
		automatedEtcdBackup bool
		tnfObservability    bool
	}{
		{name: "AutomatedEtcdBackup disabled, TNFObservability disabled"},
		{name: "AutomatedEtcdBackup disabled, TNFObservability enabled", tnfObservability: true},
		{name: "AutomatedEtcdBackup enabled, TNFObservability disabled", automatedEtcdBackup: true},
		{name: "AutomatedEtcdBackup enabled, TNFObservability enabled", automatedEtcdBackup: true, tnfObservability: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var enabledGates, disabledGates []configv1.FeatureGateName
			if tt.automatedEtcdBackup {
				enabledGates = append(enabledGates, backuphelpers.AutomatedEtcdBackupFeatureGateName)
			} else {
				disabledGates = append(disabledGates, backuphelpers.AutomatedEtcdBackupFeatureGateName)
			}
			if tt.tnfObservability {
				enabledGates = append(enabledGates, features.FeatureGateTNFObservability)
			} else {
				disabledGates = append(disabledGates, features.FeatureGateTNFObservability)
			}
			featureGateAccessor := featuregates.NewHardcodedFeatureGateAccess(enabledGates, disabledGates)
			featureGate, err := featureGateAccessor.CurrentFeatureGates()
			require.NoError(t, err)

			existingJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "existing-job", Namespace: operatorclient.TargetNamespace}}
			kubeClient := kubefake.NewSimpleClientset(existingJob)
			kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
				kubeClient,
				operatorclient.GlobalUserSpecifiedConfigNamespace,
				operatorclient.GlobalMachineSpecifiedConfigNamespace,
				operatorclient.TargetNamespace,
				operatorclient.OperatorNamespace,
				"kube-system",
			)
			configInformers := configv1informersfactory.NewSharedInformerFactory(configfake.NewSimpleClientset(&configv1.Infrastructure{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
				Status:     configv1.InfrastructureStatus{ControlPlaneTopology: configv1.DualReplicaTopologyMode},
			}), 0)
			operatorInformers := operatorinformers.NewSharedInformerFactory(operatorfake.NewSimpleClientset(), 0)
			controlPlaneNodeInformer := corev1informers.NewNodeInformer(kubeClient, 0, cache.Indexers{})
			operatorClient := &lockedStaticPodOperatorClient{StaticPodOperatorClient: v1helpers.NewFakeStaticPodOperatorClient(
				&operatorv1.StaticPodOperatorSpec{OperatorSpec: operatorv1.OperatorSpec{ManagementState: operatorv1.Managed}},
				&operatorv1.StaticPodOperatorStatus{}, nil, nil)}
			recorder := events.NewLoggingEventRecorder("tnf-startup-test", clock.RealClock{})
			controllerContext := &controllercmd.ControllerContext{
				// Clients built from this config by the TNF startup path get NotFound for
				// everything, so the PacemakerCluster CRD never becomes established.
				KubeConfig: &rest.Config{
					Host:          "https://tnf-startup-test.invalid",
					ContentConfig: rest.ContentConfig{ContentType: "application/json"},
					Transport:     crdRoundTripper(notFoundResponse),
				},
				EventRecorder: recorder,
			}

			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			defer func() {
				cancel()
				wg.Wait()
				for namespace := range kubeInformersForNamespaces.Namespaces() {
					kubeInformersForNamespaces.InformersFor(namespace).Shutdown()
				}
				operatorInformers.Shutdown()
				configInformers.Shutdown()
			}()

			// RunOperator registers these informers and starts the config informers before
			// calling HandleDualReplicaClusters.
			infrastructureInformer := configInformers.Config().V1().Infrastructures()
			infrastructureInformer.Informer()
			networkInformer := configInformers.Config().V1().Networks()
			networkInformer.Informer()
			configInformers.Start(ctx.Done())

			// With AutomatedEtcdBackup enabled, RunOperator's backup controllers register the
			// same Jobs informer before the factory starts.
			autoBackupEnabled, err := backuphelpers.AutoBackupFeatureGateEnabled(featureGateAccessor)
			require.NoError(t, err)
			var backupJobsInformer cache.SharedIndexInformer
			if autoBackupEnabled {
				backupJobsInformer = kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs().Informer()
			}

			handled, err := HandleDualReplicaClusters(ctx, controllerContext, infrastructureInformer, operatorClient,
				etcdenvvar.FakeEnvVar{}, kubeInformersForNamespaces, networkInformer, controlPlaneNodeInformer,
				operatorInformers.Operator().V1().Etcds(), kubeClient, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), featureGate)
			require.NoError(t, err)
			require.True(t, handled, "DualReplica topology must start the TNF controllers")

			// Same order as RunOperator: this is the only Start of the namespace informer factory.
			wg.Add(1)
			go func() {
				defer wg.Done()
				controlPlaneNodeInformer.Run(ctx.Done())
			}()
			operatorInformers.Start(ctx.Done())
			kubeInformersForNamespaces.Start(ctx.Done())
			configInformers.Start(ctx.Done())

			// Create a TNF job controller after the factory has started, as the lifecycle
			// manager does, and wait for its real cache sync to let the sync function run.
			jobInformer := kubeInformersForNamespaces.InformersFor(operatorclient.TargetNamespace).Batch().V1().Jobs()
			synced := make(chan struct{})
			var syncedOnce sync.Once
			jobController := jobs.NewJobController(
				"tnf-startup-test",
				bindata.MustAsset("tnfdeployment/job.yaml"),
				recorder,
				operatorClient,
				kubeClient,
				jobInformer,
				nil,
				nil,
				func(_ *operatorv1.OperatorSpec, _ *batchv1.Job) (bool, error) {
					syncedOnce.Do(func() { close(synced) })
					return false, nil
				},
			)
			wg.Add(1)
			go func() {
				defer wg.Done()
				jobController.Run(ctx, 1)
			}()

			select {
			case <-synced:
			case <-time.After(lateJobControllerSyncTimeout):
				t.Fatalf("TNF job controller created after the namespace informer factory started did not sync within %v: "+
					"the openshift-etcd Jobs informer was not registered before the factory started", lateJobControllerSyncTimeout)
			}
			require.True(t, jobInformer.Informer().HasSynced())
			_, err = jobInformer.Lister().Jobs(operatorclient.TargetNamespace).Get(existingJob.Name)
			require.NoError(t, err, "the Jobs cache must be populated from the API by list/watch")
			if backupJobsInformer != nil {
				require.Same(t, backupJobsInformer.GetIndexer(), jobInformer.Informer().GetIndexer(), "TNF and backup controllers must share one Jobs informer")
			}
		})
	}
}

// notFoundResponse answers every request with a Kubernetes NotFound status.
func notFoundResponse(req *http.Request) (*http.Response, error) {
	data, err := json.Marshal(metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Reason:   metav1.StatusReasonNotFound,
		Code:     http.StatusNotFound,
	})
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(data))),
		Request:    req,
	}, nil
}

// lockedStaticPodOperatorClient serializes access to the library-go fake operator client, which
// is not safe for the concurrent use by the controllers HandleDualReplicaClusters starts. It also
// returns empty object metadata, which the fake does not support but the job controller reads.
type lockedStaticPodOperatorClient struct {
	v1helpers.StaticPodOperatorClient
	lock sync.Mutex
}

func (c *lockedStaticPodOperatorClient) GetObjectMeta() (*metav1.ObjectMeta, error) {
	return &metav1.ObjectMeta{}, nil
}

func (c *lockedStaticPodOperatorClient) GetOperatorState() (*operatorv1.OperatorSpec, *operatorv1.OperatorStatus, string, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.GetOperatorState()
}

func (c *lockedStaticPodOperatorClient) GetOperatorStateWithQuorum(ctx context.Context) (*operatorv1.OperatorSpec, *operatorv1.OperatorStatus, string, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.GetOperatorStateWithQuorum(ctx)
}

func (c *lockedStaticPodOperatorClient) UpdateOperatorSpec(ctx context.Context, resourceVersion string, in *operatorv1.OperatorSpec) (*operatorv1.OperatorSpec, string, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.UpdateOperatorSpec(ctx, resourceVersion, in)
}

func (c *lockedStaticPodOperatorClient) UpdateOperatorStatus(ctx context.Context, resourceVersion string, in *operatorv1.OperatorStatus) (*operatorv1.OperatorStatus, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.UpdateOperatorStatus(ctx, resourceVersion, in)
}

func (c *lockedStaticPodOperatorClient) ApplyOperatorSpec(ctx context.Context, fieldManager string, cfg *applyoperatorv1.OperatorSpecApplyConfiguration) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.ApplyOperatorSpec(ctx, fieldManager, cfg)
}

func (c *lockedStaticPodOperatorClient) ApplyOperatorStatus(ctx context.Context, fieldManager string, cfg *applyoperatorv1.OperatorStatusApplyConfiguration) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.ApplyOperatorStatus(ctx, fieldManager, cfg)
}

func (c *lockedStaticPodOperatorClient) PatchOperatorStatus(ctx context.Context, patch *jsonpatch.PatchSet) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.PatchOperatorStatus(ctx, patch)
}

func (c *lockedStaticPodOperatorClient) GetStaticPodOperatorState() (*operatorv1.StaticPodOperatorSpec, *operatorv1.StaticPodOperatorStatus, string, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.GetStaticPodOperatorState()
}

func (c *lockedStaticPodOperatorClient) GetStaticPodOperatorStateWithQuorum(ctx context.Context) (*operatorv1.StaticPodOperatorSpec, *operatorv1.StaticPodOperatorStatus, string, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.GetStaticPodOperatorStateWithQuorum(ctx)
}

func (c *lockedStaticPodOperatorClient) UpdateStaticPodOperatorStatus(ctx context.Context, resourceVersion string, in *operatorv1.StaticPodOperatorStatus) (*operatorv1.StaticPodOperatorStatus, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.UpdateStaticPodOperatorStatus(ctx, resourceVersion, in)
}

func (c *lockedStaticPodOperatorClient) UpdateStaticPodOperatorSpec(ctx context.Context, resourceVersion string, in *operatorv1.StaticPodOperatorSpec) (*operatorv1.StaticPodOperatorSpec, string, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.UpdateStaticPodOperatorSpec(ctx, resourceVersion, in)
}

func (c *lockedStaticPodOperatorClient) ApplyStaticPodOperatorSpec(ctx context.Context, fieldManager string, cfg *applyoperatorv1.StaticPodOperatorSpecApplyConfiguration) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.ApplyStaticPodOperatorSpec(ctx, fieldManager, cfg)
}

func (c *lockedStaticPodOperatorClient) ApplyStaticPodOperatorStatus(ctx context.Context, fieldManager string, cfg *applyoperatorv1.StaticPodOperatorStatusApplyConfiguration) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.ApplyStaticPodOperatorStatus(ctx, fieldManager, cfg)
}

func (c *lockedStaticPodOperatorClient) PatchStaticOperatorStatus(ctx context.Context, patch *jsonpatch.PatchSet) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.StaticPodOperatorClient.PatchStaticOperatorStatus(ctx, patch)
}
