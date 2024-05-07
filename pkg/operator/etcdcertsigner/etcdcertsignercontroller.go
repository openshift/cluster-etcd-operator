package etcdcertsigner

import (
	"context"
	"fmt"
	"github.com/openshift/library-go/pkg/crypto"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/metrics"
	"strings"
	"time"

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
	quorumChecker      ceohelpers.QuorumChecker

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
		eventRecorder:         eventRecorder,
		kubeClient:            kubeClient,
		operatorClient:        operatorClient,
		masterNodeLister:      masterNodeLister,
		masterNodeSelector:    masterNodeSelector,
		secretInformer:        secretInformer,
		secretLister:          secretLister,
		secretClient:          secretClient,
		quorumChecker:         quorumChecker,
		certConfig:            certCfg,
		signerExpirationGauge: signerExpirationGauge,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("EtcdCertSignerController", syncer)

	return factory.New().ResyncEvery(time.Minute).WithInformers(
		masterNodeInformer,
		kubeInformers.InformersFor(operatorclient.GlobalUserSpecifiedConfigNamespace).Core().V1().Secrets().Informer(),
		cmInformer.Informer(),
		secretInformer.Informer(),
		operatorClient.Informer(),
	).WithSync(syncer.Sync).ToController("EtcdCertSignerController", c.eventRecorder)
}

func (c *EtcdCertSignerController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	safe, err := c.quorumChecker.IsSafeToUpdateRevision()
	if err != nil {
		return fmt.Errorf("EtcdCertSignerController can't evaluate whether quorum is safe: %w", err)
	}

	if !safe {
		return fmt.Errorf("skipping EtcdCertSignerController reconciliation due to insufficient quorum")
	}

	if err := c.syncAllMasterCertificates(ctx, syncCtx.Recorder()); err != nil {
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

func (c *EtcdCertSignerController) syncAllMasterCertificates(ctx context.Context, recorder events.Recorder) error {
	// TODO(thomas): it is of utmost importance to keep the existing signer certs for now
	// when we just create a new signer cert, the new revision does not allow the peer to join the existing two-node
	// cluster based on the old CA. Any newly rotated additions will come for free through the signerCaBundle and will work out of the box.
	// Unfortunately, we can't make use of the certrotation.RotatedSigningCASecret here because it would immediately try to rotate the existing signer.
	signerCaPair, err := tlshelpers.ReadConfigSignerCert(ctx, c.secretClient)
	if err != nil {
		return err
	}

	c.reportExpirationMetric(signerCaPair, "signer-ca")

	// EnsureConfigMapCABundle is stateful w.r.t to the configmap it manages, so we can simply add it to the bundle before the new one
	_, err = c.certConfig.signerCaBundle.EnsureConfigMapCABundle(ctx, signerCaPair)
	if err != nil {
		return fmt.Errorf("error on ensuring signer bundle for existing pair: %w", err)
	}

	// TODO(thomas): we need to transition that new signer as a replacement for the above - today we only bundle it
	newSignerCaPair, _, err := c.certConfig.signerCert.EnsureSigningCertKeyPair(ctx)
	if err != nil {
		return fmt.Errorf("error on ensuring etcd-signer cert: %w", err)
	}

	signerBundle, err := c.certConfig.signerCaBundle.EnsureConfigMapCABundle(ctx, newSignerCaPair)
	if err != nil {
		return fmt.Errorf("error on ensuring signer bundle for new pair: %w", err)
	}

	_, err = c.certConfig.etcdClientCert.EnsureTargetCertKeyPair(ctx, signerCaPair, signerBundle)
	if err != nil {
		return fmt.Errorf("error on ensuring etcd client cert: %w", err)
	}

	metricsSignerCaPair, err := tlshelpers.ReadConfigMetricsSignerCert(ctx, c.secretClient)
	if err != nil {
		return err
	}

	c.reportExpirationMetric(metricsSignerCaPair, "metrics-signer-ca")

	_, err = c.certConfig.metricsSignerCaBundle.EnsureConfigMapCABundle(ctx, metricsSignerCaPair)
	if err != nil {
		return fmt.Errorf("error on ensuring metrics signer bundle for existing pair: %w", err)
	}

	// TODO(thomas): we need to transition that new signer as a replacement for the above - today we only bundle it
	newMetricsSignerCaPair, _, err := c.certConfig.metricsSignerCert.EnsureSigningCertKeyPair(ctx)
	if err != nil {
		return fmt.Errorf("error on ensuring metrics-signer cert: %w", err)
	}

	metricsSignerBundle, err := c.certConfig.metricsSignerCaBundle.EnsureConfigMapCABundle(ctx, newMetricsSignerCaPair)
	if err != nil {
		return fmt.Errorf("error on ensuring metrics signer bundle: %w", err)
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

	//check the quorum in case the cluster is healthy or not after generating certs
	safe, err := c.quorumChecker.IsSafeToUpdateRevision()
	if err != nil {
		return fmt.Errorf("EtcdCertSignerController can't evaluate whether quorum is safe: %w", err)
	}

	if !safe {
		return fmt.Errorf("skipping EtcdCertSignerController reconciliation due to insufficient quorum")
	}
	_, _, err = resourceapply.ApplySecret(ctx, c.secretClient, recorder, secret)

	return err
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
