package etcdcertsigner

import (
	"context"
	"crypto/x509"
	"fmt"
	"github.com/openshift/library-go/pkg/crypto"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"strconv"
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
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
)

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

// signerRotationResult contains the designated bundles and CAs used for the leaf certs
type signerRotationResult struct {
	signerRotated bool

	signingCertKeyPair        *crypto.CA
	metricsSigningCertKeyPair *crypto.CA

	signerCaBundle        []*x509.Certificate
	metricsSignerCaBundle []*x509.Certificate
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
	// forceLeafRegen will force the leafs to be generated in the same revision as the signers rotated
	// this is used for offline scenarios as seen in the render command
	forceLeafRegen bool
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
	forceLeafRegen bool,
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

	c := &EtcdCertSignerController{
		eventRecorder:      eventRecorder,
		kubeClient:         kubeClient,
		operatorClient:     operatorClient,
		masterNodeLister:   masterNodeLister,
		masterNodeSelector: masterNodeSelector,
		secretInformer:     secretInformer,
		secretLister:       secretLister,
		secretClient:       secretClient,
		quorumChecker:      quorumChecker,
		certConfig:         certCfg,
		forceLeafRegen:     forceLeafRegen,
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

	_, currentStatus, _, err := c.operatorClient.GetStaticPodOperatorState()
	if currentStatus == nil || err != nil {
		return fmt.Errorf("skipping EtcdCertSignerController can't get current static pod status: %w", err)
	}

	if ceohelpers.RevisionRolloutInProgress(*currentStatus) {
		klog.V(4).Infof("skipping EtcdCertSignerController revision rollout in progress")
		return nil
	}

	currentRevision, err := ceohelpers.CurrentRevision(*currentStatus)
	if err != nil {
		return fmt.Errorf("skipping EtcdCertSignerController can't get current revision: %w", err)
	}

	if err := c.syncAllMasterCertificates(ctx, syncCtx.Recorder(), currentStatus, currentRevision); err != nil {
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

func (c *EtcdCertSignerController) syncAllMasterCertificates(ctx context.Context, recorder events.Recorder, status *operatorv1.StaticPodOperatorStatus, currentRevision int32) error {
	lastSignerRotationRev, err := getCertRotationRevision(status, tlshelpers.EtcdSignerCertSecretName)
	if err != nil {
		return err
	}

	lastMetricsSignerRotationRev, err := getCertRotationRevision(status, tlshelpers.EtcdMetricsSignerCertSecretName)
	if err != nil {
		return err
	}

	signers, err := c.maybeRotateSigners(ctx, lastSignerRotationRev, lastMetricsSignerRotationRev, currentRevision)
	if err != nil {
		return err
	}

	if !c.forceLeafRegen && (signers.signerRotated || (currentRevision <= lastSignerRotationRev && currentRevision <= lastMetricsSignerRotationRev)) {
		klog.Infof("EtcdCertSignerController skipping leaf certificate regeneration: just_rotated=%v, current_rev=%d, last_signer_rota=%d, last_metric_signer_rota=%d",
			signers.signerRotated, currentRevision, lastSignerRotationRev, lastMetricsSignerRotationRev)
		return nil
	}

	// -----------------------------------------------------------------
	// Leaf Certificates
	// -----------------------------------------------------------------

	err = c.ensureClientCerts(ctx, signers.metricsSigningCertKeyPair, signers.metricsSignerCaBundle, signers.signingCertKeyPair, signers.signerCaBundle)
	if err != nil {
		return fmt.Errorf("error on client certs: %w", err)
	}

	nodeCfgs, err := c.createNodeCertConfigs()
	if err != nil {
		return fmt.Errorf("error while creating cert configs for nodes: %w", err)
	}

	allCerts := map[string][]byte{}
	var errs []error
	for _, cfg := range nodeCfgs {
		secret, err := cfg.peerCert.EnsureTargetCertKeyPair(ctx, signers.signingCertKeyPair, signers.signerCaBundle)
		if err != nil {
			errs = append(errs, fmt.Errorf("error on peer cert sync: %w", err))
		}
		allCerts = addCertSecretToMap(allCerts, secret)

		secret, err = cfg.servingCert.EnsureTargetCertKeyPair(ctx, signers.signingCertKeyPair, signers.signerCaBundle)
		if err != nil {
			errs = append(errs, fmt.Errorf("error on serving cert sync: %w", err))
		}
		allCerts = addCertSecretToMap(allCerts, secret)

		secret, err = cfg.metricsCert.EnsureTargetCertKeyPair(ctx, signers.metricsSigningCertKeyPair, signers.metricsSignerCaBundle)
		if err != nil {
			errs = append(errs, fmt.Errorf("error on serving metrics cert sync: %w", err))
		}
		allCerts = addCertSecretToMap(allCerts, secret)
	}

	if len(errs) > 0 {
		return fmt.Errorf("encountered errors while syncing some certificates: %w", utilerrors.NewAggregate(errs))
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
	_, _, err = resourceapply.ApplySecret(ctx, c.secretClient, recorder, secret)

	return err
}

func (c *EtcdCertSignerController) maybeRotateSigners(ctx context.Context, lastSignerRotationRev, lastMetricsSignerRotationRev, currentRevision int32) (*signerRotationResult, error) {
	// ---------------
	// server section
	// ---------------
	newSignerCaPair, signerUpdated, err := c.certConfig.signerCert.EnsureSigningCertKeyPair(ctx)
	if err != nil {
		return nil, fmt.Errorf("error on ensuring etcd-signer cert: %w", err)
	}

	if signerUpdated {
		// TODO(thomas): how can we re-create this condition when this operation fails?
		if err := c.updateCertRevision(ctx, c.certConfig.signerCert.Name, currentRevision); err != nil {
			return nil, fmt.Errorf("error while updating status rev: %w", err)
		}
	}

	res := &signerRotationResult{
		signerRotated:      signerUpdated,
		signingCertKeyPair: newSignerCaPair,
	}

	// the revision is only set when we have successfully created a new signer certificate, so for the first time we have to
	// bundle the existing signer and use it to sign any leaf certificates
	if lastSignerRotationRev == 0 {
		signerCaPair, err := tlshelpers.ReadConfigSignerCert(ctx, c.secretClient)
		if err != nil {
			return nil, err
		}

		// TODO(thomas): we need to have a follow-up to remove unused public keys from the bundle again
		_, err = c.certConfig.signerCaBundle.EnsureConfigMapCABundle(ctx, signerCaPair)
		if err != nil {
			return nil, fmt.Errorf("error on ensuring signer bundle for openshift-config pair: %w", err)
		}

		// use the old openshift-config as the signer
		res.signingCertKeyPair = signerCaPair
	}

	signerBundle, err := c.certConfig.signerCaBundle.EnsureConfigMapCABundle(ctx, newSignerCaPair)
	if err != nil {
		return nil, fmt.Errorf("error on ensuring signer bundle for openshift-etcd pair: %w", err)
	}
	res.signerCaBundle = signerBundle

	// ---------------
	// metrics section
	// ---------------

	newMetricsSignerCaPair, metricsSignerUpdated, err := c.certConfig.metricsSignerCert.EnsureSigningCertKeyPair(ctx)
	if err != nil {
		return nil, fmt.Errorf("error on ensuring metrics-signer cert: %w", err)
	}
	res.metricsSigningCertKeyPair = newMetricsSignerCaPair
	res.signerRotated = res.signerRotated || metricsSignerUpdated

	if metricsSignerUpdated {
		// TODO(thomas): how can we re-create this condition when this operation fails?
		if err := c.updateCertRevision(ctx, c.certConfig.metricsSignerCert.Name, currentRevision); err != nil {
			return nil, fmt.Errorf("error while updating status rev: %w", err)
		}

	}

	if lastMetricsSignerRotationRev == 0 {
		metricsSignerCaPair, err := tlshelpers.ReadConfigMetricsSignerCert(ctx, c.secretClient)
		if err != nil {
			return nil, err
		}

		// TODO(thomas): we need to have a follow-up to remove unused public keys from the bundle again
		_, err = c.certConfig.metricsSignerCaBundle.EnsureConfigMapCABundle(ctx, metricsSignerCaPair)
		if err != nil {
			return nil, fmt.Errorf("error on ensuring metrics signer bundle for openshift-config pair: %w", err)
		}
		res.metricsSigningCertKeyPair = metricsSignerCaPair
	}
	metricsSignerBundle, err := c.certConfig.metricsSignerCaBundle.EnsureConfigMapCABundle(ctx, newMetricsSignerCaPair)
	if err != nil {
		return nil, fmt.Errorf("error on ensuring metrics signer bundle for openshift-etcd pair: %w", err)
	}
	res.metricsSignerCaBundle = metricsSignerBundle

	return res, nil
}

func (c *EtcdCertSignerController) ensureClientCerts(ctx context.Context, metricsSignerCaPair *crypto.CA, metricsSignerBundle []*x509.Certificate, signerCaPair *crypto.CA, signerBundle []*x509.Certificate) error {
	_, err := c.certConfig.metricsClientCert.EnsureTargetCertKeyPair(ctx, metricsSignerCaPair, metricsSignerBundle)
	if err != nil {
		return fmt.Errorf("error on ensuring metrics client cert: %w", err)
	}

	_, err = c.certConfig.etcdClientCert.EnsureTargetCertKeyPair(ctx, signerCaPair, signerBundle)
	if err != nil {
		return fmt.Errorf("error on ensuring etcd client cert: %w", err)
	}
	return nil
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

func (c *EtcdCertSignerController) updateCertRevision(ctx context.Context, certSecretName string, revision int32) error {
	_, _, err := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:    fmt.Sprintf("EtcdCertSignerController-rotation-rev-%s", certSecretName),
		Status:  operatorv1.ConditionTrue,
		Message: fmt.Sprintf("%d", revision),
	}))

	return err
}

func getCertRotationRevision(status *operatorv1.StaticPodOperatorStatus, certSecretName string) (int32, error) {
	condition := v1helpers.FindOperatorCondition(status.Conditions, fmt.Sprintf("EtcdCertSignerController-rotation-rev-%s", certSecretName))
	if condition == nil {
		return int32(0), nil
	}

	rev, err := strconv.ParseInt(condition.Message, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("could not parse condition message for cert secret [%s], msg=[%s]: %w", certSecretName, condition.Message, err)
	}
	return int32(rev), nil
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
