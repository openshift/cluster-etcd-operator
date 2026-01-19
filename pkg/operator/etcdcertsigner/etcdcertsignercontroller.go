package etcdcertsigner

import (
	"context"
	"crypto/x509"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/bootstrap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/metrics"
	"k8s.io/klog/v2"

	apiannotations "github.com/openshift/api/annotations"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/certrotation"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
)

const BundleRolloutRevisionAnnotation = "openshift.io/ceo-bundle-rollout-revision"
const signerExpirationMetricName = "openshift_etcd_operator_signer_expiration_days"

type certConfig struct {
	// configmap name: "etcd-ca-bundle"
	signerCaBundle certrotation.CABundleConfigMap
	// secret name: "etcd-signer"
	signerCert certrotation.RotatedSigningCASecret

	// configmap name: "etcd-metric-ca-bundle"
	metricsSignerCaBundle certrotation.CABundleConfigMap
	// secret name: "etcd-metric-signer"
	metricsSignerCert certrotation.RotatedSigningCASecret

	// secret name: "etcd-metric-client"
	metricsClientCert certrotation.RotatedSelfSignedCertKeySecret
	// secret name: "etcd-client"
	etcdClientCert certrotation.RotatedSelfSignedCertKeySecret
}

// nodeCertConfigs contains all certificates generated per node
type nodeCertConfigs struct {
	node *corev1.Node

	peerCert    *certrotation.RotatedSelfSignedCertKeySecret
	servingCert *certrotation.RotatedSelfSignedCertKeySecret
	metricsCert *certrotation.RotatedSelfSignedCertKeySecret
}

type EtcdCertSignerController struct {
	eventRecorder      events.Recorder
	kubeClient         kubernetes.Interface
	operatorClient     v1helpers.StaticPodOperatorClient
	masterNodeLister   corev1listers.NodeLister
	masterNodeSelector labels.Selector
	secretInformer     corev1informers.SecretInformer
	secretLister       corev1listers.SecretLister
	secretClient       corev1client.SecretsGetter
	configMapClient    corev1client.ConfigMapsGetter
	configmapLister    corev1listers.ConfigMapLister
	quorumChecker      ceohelpers.QuorumChecker

	// when true we skip all checks related to the rollout of static pods, this is used in render
	forceSkipRollout bool

	certConfig *certConfig

	// metrics
	signerExpirationGauge *metrics.GaugeVec
}

// NewEtcdCertSignerController watches master nodes and maintains secrets for each master node, placing them in a single secret (NOT a tls secret)
// so that the revision controller only has to watch a single secret.  This isn't ideal because it's possible to have a
// revision that is missing the content of a secret, but the actual static pod will fail if that happens and the later
// revision will pick it up.
func NewEtcdCertSignerController(
	livenessChecker *health.MultiAlivenessChecker,
	kubeClient kubernetes.Interface,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	masterNodeInformer cache.SharedIndexInformer,
	masterNodeLister corev1listers.NodeLister,
	masterNodeSelector labels.Selector,
	eventRecorder events.Recorder,
	quorumChecker ceohelpers.QuorumChecker,
	metricsRegistry metrics.KubeRegistry,
	forceSkipRollout bool,
) factory.Controller {
	eventRecorder = eventRecorder.WithComponentSuffix("etcd-cert-signer-controller")
	cmInformer := kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().ConfigMaps()
	cmGetter := kubeClient.CoreV1()
	cmLister := &tlshelpers.ConfigMapClientLister{
		ConfigMapClient: cmGetter.ConfigMaps(operatorclient.TargetNamespace),
		Namespace:       operatorclient.TargetNamespace,
	}
	signerCaBundle := tlshelpers.CreateSignerCertRotationBundleConfigMap(
		cmInformer,
		cmLister,
		cmGetter,
		eventRecorder,
	)

	metricsSignerCaBundle := tlshelpers.CreateMetricsSignerCertRotationBundleConfigMap(
		cmInformer,
		cmLister,
		cmGetter,
		eventRecorder,
	)

	secretInformer := kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets()
	secretClient := kubeClient.CoreV1()
	secretLister := &tlshelpers.SecretClientLister{
		SecretClient: secretClient.Secrets(operatorclient.TargetNamespace),
		Namespace:    operatorclient.TargetNamespace,
	}
	signerCert := tlshelpers.CreateSignerCert(secretInformer, secretLister, secretClient, eventRecorder)
	etcdClientCert := tlshelpers.CreateEtcdClientCert(secretInformer, secretLister, secretClient, eventRecorder)

	metricsSignerCert := tlshelpers.CreateMetricsSignerCert(secretInformer, secretLister, secretClient, eventRecorder)
	metricsClientCert := tlshelpers.CreateMetricsClientCert(secretInformer, secretLister, secretClient, eventRecorder)

	certCfg := &certConfig{
		signerCaBundle: signerCaBundle,
		signerCert:     signerCert,
		etcdClientCert: etcdClientCert,

		metricsSignerCaBundle: metricsSignerCaBundle,
		metricsSignerCert:     metricsSignerCert,
		metricsClientCert:     metricsClientCert,
	}

	signerExpirationGauge := metrics.NewGaugeVec(&metrics.GaugeOpts{
		Name: signerExpirationMetricName,
		Help: "Report observed days to expiration of a given signer certificate over time",
	}, []string{"name"})
	metricsRegistry.MustRegister(signerExpirationGauge)

	c := &EtcdCertSignerController{
		eventRecorder:      eventRecorder,
		kubeClient:         kubeClient,
		operatorClient:     operatorClient,
		masterNodeLister:   masterNodeLister,
		masterNodeSelector: masterNodeSelector,
		secretInformer:     secretInformer,
		secretLister:       secretLister,
		secretClient:       secretClient,
		configMapClient:    cmGetter,
		// this one can go through the informers, it's only used for bootstrap checks
		configmapLister:       kubeInformers.InformersFor(operatorclient.KubeSystemNamespace).Core().V1().ConfigMaps().Lister(),
		quorumChecker:         quorumChecker,
		certConfig:            certCfg,
		signerExpirationGauge: signerExpirationGauge,
		forceSkipRollout:      forceSkipRollout,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("EtcdCertSignerController", syncer)

	return factory.New().ResyncEvery(time.Minute).WithInformers(
		masterNodeInformer,
		kubeInformers.InformersFor(operatorclient.GlobalUserSpecifiedConfigNamespace).Core().V1().Secrets().Informer(),
		kubeInformers.InformersFor(operatorclient.KubeSystemNamespace).Core().V1().ConfigMaps().Informer(),
		cmInformer.Informer(),
		secretInformer.Informer(),
		operatorClient.Informer(),
	).WithSync(syncer.Sync).ToController("EtcdCertSignerController", c.eventRecorder)
}

func (c *EtcdCertSignerController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	bootstrapComplete, err := bootstrap.IsBootstrapComplete(c.configmapLister)
	if err != nil {
		return err
	}

	// we allow the controller to run freely during bootstrap to avoid having issues with constantly rolling out revisions and other
	// contention issues on the operator status update.
	if !bootstrapComplete {
		if err := c.syncAllMasterCertificates(ctx, syncCtx.Recorder(), true, 0, 0); err != nil {
			return fmt.Errorf("EtcdCertSignerController failed to sync all master certificates during bootstrap: %w", err)
		}
		return nil
	}

	// If the etcd-all-bundles configmap is missing, the revision rollout can become deadlocked as the staticpod installer pod won't find
	// the etcd-all-bundles configmap to install on disk and keep failing.
	// Meanwhile the CertSignerController will keep skipping on account of an ongoing revision due to the revision gate for
	// regenerating the leaf certs below.
	// As in the case of bootstrap, we let the controller run freely to regenerate the etcd-all-bundles configmap and allow the next revision to install.
	// TODO(haseeb): Clarify how the etcd-all-bundles confimap may be missing. Also does this apply to secret/etcd-all-certs as well?
	missingAllBundlesConfigmap, err := c.isMissingAllBundleConfigmap(ctx)
	if err != nil {
		return err
	}

	if missingAllBundlesConfigmap {
		klog.Warningf("could not find %s configmap, forcing EtcdCertSignerController sync to regenerate", tlshelpers.EtcdAllBundlesConfigMapName)
		if err := c.syncAllMasterCertificates(ctx, syncCtx.Recorder(), true, 0, 0); err != nil {
			return fmt.Errorf("EtcdCertSignerController failed to sync all master certificates to regenerate missing %s configmap: %w", tlshelpers.EtcdAllBundlesConfigMapName, err)
		}
		return nil
	}

	hasNodeDiff, err := c.hasNodeCertDiff()
	if err != nil {
		return err
	}

	if hasNodeDiff {
		klog.Infof("EtcdCertSignerController force leaf sync on node difference")
		if err := c.forcedSyncLeafCertificates(ctx, syncCtx.Recorder()); err != nil {
			return fmt.Errorf("EtcdCertSignerController failed to force sync leaf certificates: %w", err)
		}

		return nil
	}

	_, currentStatus, _, err := c.operatorClient.GetStaticPodOperatorStateWithQuorum(ctx)
	if err != nil || currentStatus == nil {
		return fmt.Errorf("skipping EtcdCertSignerController can't get current status: %w", err)
	}

	currentRevision, err := ceohelpers.CurrentRevision(*currentStatus)
	if err != nil {
		// we explicitly do not return error here, as this will degrade the operator during any revision rollout.
		klog.Infof("skipping EtcdCertSignerController can't get current revision. Err=%v", err)
		return nil
	}

	rotationRevision, err := c.getCertRotationRevision(ctx)
	if err != nil {
		return err
	}

	if err := c.syncAllMasterCertificates(ctx, syncCtx.Recorder(), c.forceSkipRollout, rotationRevision, currentRevision); err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdCertSignerControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("EtcdCertSignerControllerUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "EtcdCertSignerControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *EtcdCertSignerController) syncAllMasterCertificates(
	ctx context.Context, recorder events.Recorder, forceSkipRollout bool, lastRotationRevision int32, currentRevision int32) error {
	signerCaPair, _, err := c.certConfig.signerCert.EnsureSigningCertKeyPair(ctx)
	if err != nil {
		return fmt.Errorf("error on ensuring etcd-signer cert: %w", err)
	}
	c.reportExpirationMetric(signerCaPair, "signer-ca")

	metricsSignerCaPair, _, err := c.certConfig.metricsSignerCert.EnsureSigningCertKeyPair(ctx)
	if err != nil {
		return fmt.Errorf("error on ensuring metrics-signer cert: %w", err)
	}
	c.reportExpirationMetric(metricsSignerCaPair, "metrics-signer-ca")

	signerBundle, metricsSignerBundle, rolloutTriggered, err := c.ensureBundles(ctx, recorder, forceSkipRollout, signerCaPair, metricsSignerCaPair, currentRevision)
	if err != nil {
		return fmt.Errorf("error on ensuring bundles: %w", err)
	}

	if !forceSkipRollout {
		if rolloutTriggered {
			klog.Infof("skipping EtcdCertSignerController leaf cert generation as revision rollout has been triggered")
			return nil
		}

		if currentRevision <= lastRotationRevision {
			klog.Infof("skipping EtcdCertSignerController leaf cert generation as safe revision is not yet achieved, currently at %d - rotation happend at %d", currentRevision, lastRotationRevision)
			return nil
		}
	}

	return c.syncLeafCertificates(ctx, recorder, forceSkipRollout, signerCaPair, signerBundle, metricsSignerCaPair, metricsSignerBundle)
}

// forcedSyncLeafCertificates will ensure we only read signer certificates and bundles, never re-create them.
// This ensures that on node scale-ups we never issue a new signer due to expiration and potentially brick a cluster.
func (c *EtcdCertSignerController) forcedSyncLeafCertificates(ctx context.Context, recorder events.Recorder) error {
	signerCert, err := tlshelpers.ReadSignerCaCert(ctx, c.secretClient, tlshelpers.EtcdSignerCertSecretName)
	if err != nil {
		return fmt.Errorf("error while reading signer ca during forced leaf sync: %w", err)
	}

	signerBundle, err := tlshelpers.ReadSignerCaBundle(ctx, c.configMapClient, tlshelpers.EtcdSignerCaBundleConfigMapName)
	if err != nil {
		return fmt.Errorf("error while reading signer ca bundle during forced leaf sync: %w", err)
	}

	metricsCert, err := tlshelpers.ReadSignerCaCert(ctx, c.secretClient, tlshelpers.EtcdMetricsSignerCertSecretName)
	if err != nil {
		return fmt.Errorf("error while reading metrics signer ca during forced leaf sync: %w", err)
	}

	metricsBundle, err := tlshelpers.ReadSignerCaBundle(ctx, c.configMapClient, tlshelpers.EtcdMetricsSignerCaBundleConfigMapName)
	if err != nil {
		return fmt.Errorf("error while reading metrics signer ca bundle during forced leaf sync: %w", err)
	}

	return c.syncLeafCertificates(ctx, recorder, true, signerCert, signerBundle, metricsCert, metricsBundle)
}

func (c *EtcdCertSignerController) syncLeafCertificates(
	ctx context.Context,
	recorder events.Recorder,
	forceSkipRollout bool,
	signerCaPair *crypto.CA,
	signerBundle []*x509.Certificate,
	metricsSignerCaPair *crypto.CA,
	metricsSignerBundle []*x509.Certificate) error {

	_, err := c.certConfig.etcdClientCert.EnsureTargetCertKeyPair(ctx, signerCaPair, signerBundle)
	if err != nil {
		return fmt.Errorf("error on ensuring etcd client cert: %w", err)
	}
	_, err = c.certConfig.metricsClientCert.EnsureTargetCertKeyPair(ctx, metricsSignerCaPair, metricsSignerBundle)
	if err != nil {
		return fmt.Errorf("error on ensuring metrics client cert: %w", err)
	}

	nodeCfgs, err := c.createNodeCertConfigs()
	if err != nil {
		return fmt.Errorf("error while creating cert configs for nodes: %w", err)
	}

	allCerts := map[string][]byte{}
	for _, cfg := range nodeCfgs {
		secret, err := cfg.peerCert.EnsureTargetCertKeyPair(ctx, signerCaPair, signerBundle)
		if err != nil {
			return fmt.Errorf("error on peer cert sync for node %s: %w", cfg.node.Name, err)
		}
		allCerts = addCertSecretToMap(allCerts, secret)

		secret, err = cfg.servingCert.EnsureTargetCertKeyPair(ctx, signerCaPair, signerBundle)
		if err != nil {
			return fmt.Errorf("error on serving cert sync for node %s: %w", cfg.node.Name, err)
		}
		allCerts = addCertSecretToMap(allCerts, secret)

		secret, err = cfg.metricsCert.EnsureTargetCertKeyPair(ctx, metricsSignerCaPair, metricsSignerBundle)
		if err != nil {
			return fmt.Errorf("error on serving metrics cert sync for node %s: %w", cfg.node.Name, err)
		}
		allCerts = addCertSecretToMap(allCerts, secret)
	}

	// Write a secret that aggregates all certs for all nodes for the static
	// pod controller to watch. A single secret ensures that a cert change
	// (e.g. node addition or cert rotation) triggers at most one static pod
	// rollout. If multiple secrets were written, the static pod controller
	// might initiate rollout before all secrets had been updated.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: operatorclient.TargetNamespace,
			Name:      tlshelpers.EtcdAllCertsSecretName,
			Annotations: map[string]string{
				apiannotations.OpenShiftComponent: "Etcd",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: allCerts,
	}

	// check the quorum in case the cluster is healthy or not after generating certs, unless we're in force mode
	if !forceSkipRollout {
		safe, err := c.quorumChecker.IsSafeToUpdateRevision()
		if err != nil {
			return fmt.Errorf("EtcdCertSignerController can't evaluate whether quorum is safe: %w", err)
		}

		if !safe {
			return fmt.Errorf("skipping EtcdCertSignerController reconciliation due to insufficient quorum")
		}
	}
	_, _, err = resourceapply.ApplySecret(ctx, c.secretClient, recorder, secret)
	return err
}

// ensureBundles will take the metrics and server CA certificates and ensure the bundle is updated.
// This will cause a revision rollout if a change in the bundle was detected.
func (c *EtcdCertSignerController) ensureBundles(ctx context.Context,
	recorder events.Recorder,
	forceSkipRollout bool,
	serverCA *crypto.CA,
	metricsCA *crypto.CA,
	currentRevision int32,
) (serverBundle []*x509.Certificate, metricsBundle []*x509.Certificate, rolloutTriggered bool, err error) {
	serverBundle, err = c.certConfig.signerCaBundle.EnsureConfigMapCABundle(ctx, serverCA, tlshelpers.EtcdSignerCaBundleConfigMapName)
	if err != nil {
		return nil, nil, false, err
	}

	serverCaBytes, err := crypto.EncodeCertificates(serverBundle...)
	if err != nil {
		return nil, nil, false, fmt.Errorf("could not encode server bundle: %w", err)
	}

	metricsBundle, err = c.certConfig.metricsSignerCaBundle.EnsureConfigMapCABundle(ctx, metricsCA, tlshelpers.EtcdMetricsSignerCaBundleConfigMapName)
	if err != nil {
		return nil, nil, false, err
	}

	metricsCaBytes, err := crypto.EncodeCertificates(metricsBundle...)
	if err != nil {
		return nil, nil, false, fmt.Errorf("could not encode metrics bundle: %w", err)
	}

	// Write a configmap that aggregates all certs for all nodes for the static
	// pod controller to watch. A single configmap ensures that a bundle change
	// (e.g. node addition or cert rotation) triggers at most one static pod
	// rollout. If multiple configmaps were written, the static pod controller
	// might initiate rollout before all configmaps had been updated.
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: operatorclient.TargetNamespace,
			Name:      tlshelpers.EtcdAllBundlesConfigMapName,
			Annotations: map[string]string{
				apiannotations.OpenShiftComponent: "Etcd",
			},
		},
		Data: map[string]string{
			"server-ca-bundle.crt":  string(serverCaBytes),
			"metrics-ca-bundle.crt": string(metricsCaBytes),
		},
	}

	latestConfigMap, err := c.configMapClient.ConfigMaps(operatorclient.TargetNamespace).Get(ctx, tlshelpers.EtcdAllBundlesConfigMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			latestConfigMap = &corev1.ConfigMap{Data: map[string]string{}}
		} else {
			return nil, nil, false, fmt.Errorf("could not get current %s configmap %w", tlshelpers.EtcdAllBundlesConfigMapName, err)
		}
	}

	// we can skip the reconciliation here early when there's no change in the bundles
	if reflect.DeepEqual(latestConfigMap.Data, configMap.Data) {
		return
	}

	// this ensures we always tag the right revision we're rolling out with, so we can later ensure
	// the leaf certificates are not regenerated too early.
	configMap.Annotations[BundleRolloutRevisionAnnotation] = fmt.Sprintf("%d", currentRevision)

	// The rollout may be stuck due to a missing etcd-all-bundles configmap, so we override the quorum check here to regenerate the configmap and let the next revision install
	if !forceSkipRollout {
		safe, err := c.quorumChecker.IsSafeToUpdateRevision()
		if err != nil {
			return nil, nil, false, fmt.Errorf("EtcdCertSignerController.ensureBundles can't evaluate whether quorum is safe: %w", err)
		}
		if !safe {
			return nil, nil, false, fmt.Errorf("skipping EtcdCertSignerController.ensureBundles reconciliation due to insufficient quorum")
		}
	}

	_, rolloutTriggered, err = resourceapply.ApplyConfigMap(ctx, c.configMapClient, recorder, configMap)
	if err != nil {
		return nil, nil, rolloutTriggered, fmt.Errorf("could not apply bundle configmap: %w", err)
	}
	return
}

// Nodes change internally the whole time (e.g. due to IPs changing), we thus re-create the cert configs every sync loop.
// This works, because initialization is cheap and all state is kept in secrets, configmaps and their annotations.
func (c *EtcdCertSignerController) createNodeCertConfigs() ([]*nodeCertConfigs, error) {
	var cfgs []*nodeCertConfigs
	nodes, err := c.masterNodeLister.List(c.masterNodeSelector)
	if err != nil {
		return cfgs, err
	}

	for _, node := range nodes {
		peerCert, err := tlshelpers.CreatePeerCertificate(node,
			c.secretInformer,
			c.secretLister,
			c.secretClient,
			c.eventRecorder)
		if err != nil {
			return cfgs, fmt.Errorf("error creating peer cert for node [%s]: %w", node.Name, err)
		}

		servingCert, err := tlshelpers.CreateServingCertificate(node,
			c.secretInformer,
			c.secretLister,
			c.secretClient,
			c.eventRecorder)
		if err != nil {
			return cfgs, fmt.Errorf("error creating serving cert for node [%s]: %w", node.Name, err)
		}

		metricsCert, err := tlshelpers.CreateMetricsServingCertificate(node,
			c.secretInformer,
			c.secretLister,
			c.secretClient,
			c.eventRecorder)
		if err != nil {
			return cfgs, fmt.Errorf("error creating metrics cert for node [%s]: %w", node.Name, err)
		}

		cfgs = append(cfgs, &nodeCertConfigs{
			node:        node.DeepCopy(),
			peerCert:    peerCert,
			servingCert: servingCert,
			metricsCert: metricsCert,
		})
	}

	return cfgs, nil
}

func (c *EtcdCertSignerController) reportExpirationMetric(pair *crypto.CA, name string) {
	expDate := pair.Config.Certs[0].NotAfter
	daysUntil := expDate.Sub(time.Now()).Hours() / 24
	c.signerExpirationGauge.WithLabelValues(name).Set(daysUntil)
}

func (c *EtcdCertSignerController) hasNodeCertDiff() (bool, error) {
	nodes, err := c.masterNodeLister.List(c.masterNodeSelector)
	if err != nil {
		return false, err
	}

	allSecrets, err := c.secretLister.Secrets(operatorclient.TargetNamespace).Get(tlshelpers.EtcdAllCertsSecretName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("could not find secret [%s]", tlshelpers.EtcdAllCertsSecretName)
			return true, nil
		}
		return false, err
	}

	for _, node := range nodes {
		secretDataKey := fmt.Sprintf("%s.key", tlshelpers.GetServingSecretNameForNode(node.Name))
		if _, ok := allSecrets.Data[secretDataKey]; !ok {
			klog.Infof("could not find serving cert for node [%s] and key [%s] in bundled secret", node.Name, secretDataKey)
			return true, nil
		}
	}
	return false, nil
}

func (c *EtcdCertSignerController) getCertRotationRevision(ctx context.Context) (int32, error) {
	latestConfigMap, err := c.configMapClient.ConfigMaps(operatorclient.TargetNamespace).Get(ctx, tlshelpers.EtcdAllBundlesConfigMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return int32(0), nil
		}

		return 0, fmt.Errorf("could not get all-bundle configmap: %w", err)
	}

	if v, ok := latestConfigMap.Annotations[BundleRolloutRevisionAnnotation]; ok {
		rev, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("could not parse annotation for cert rotation revision [%s]: %w", v, err)
		}

		return int32(rev), nil
	}

	return int32(0), nil
}

func (c *EtcdCertSignerController) isMissingAllBundleConfigmap(ctx context.Context) (bool, error) {
	_, err := c.configMapClient.ConfigMaps(operatorclient.TargetNamespace).Get(ctx, tlshelpers.EtcdAllBundlesConfigMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("could not get current %s configmap %w", tlshelpers.EtcdAllBundlesConfigMapName, err)
	}
	return false, nil
}

func addCertSecretToMap(allCerts map[string][]byte, secret *corev1.Secret) map[string][]byte {
	for k, v := range secret.Data {
		// in library-go the certs are stored as tls.crt and tls.key - which we trim away to stay backward compatible
		ext, found := strings.CutPrefix(k, "tls")
		if found {
			k = ext
		}
		allCerts[fmt.Sprintf("%s%s", secret.Name, k)] = v
	}
	return allCerts
}
