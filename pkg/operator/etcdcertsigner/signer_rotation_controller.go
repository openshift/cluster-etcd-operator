package etcdcertsigner

import (
	"context"
	"fmt"
	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/client-go/kubernetes"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

type RotationController struct {
	eventRecorder  events.Recorder
	kubeClient     kubernetes.Interface
	operatorClient v1helpers.StaticPodOperatorClient
	secretInformer corev1informers.SecretInformer
	secretLister   corev1listers.SecretLister
	secretClient   corev1client.SecretsGetter
	quorumChecker  ceohelpers.QuorumChecker
}

func NewCertSignerRotationController(
	livenessChecker *health.MultiAlivenessChecker,
	kubeClient kubernetes.Interface,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	eventRecorder events.Recorder,
	quorumChecker ceohelpers.QuorumChecker,
) factory.Controller {
	eventRecorder = eventRecorder.WithComponentSuffix("etcd-cert-signer-rotation-process")
	secretInformer := kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets()
	secretLister := secretInformer.Lister()
	secretClient := v1helpers.CachedSecretGetter(kubeClient.CoreV1(), kubeInformers)

	c := &RotationController{
		eventRecorder:  eventRecorder,
		kubeClient:     kubeClient,
		operatorClient: operatorClient,
		secretInformer: secretInformer,
		secretLister:   secretLister,
		secretClient:   secretClient,
		quorumChecker:  quorumChecker,
	}

	syncer := health.NewDefaultCheckingSyncWrapper(c.sync)
	livenessChecker.Add("EtcdCertSignerRotationController", syncer)

	return factory.New().ResyncEvery(time.Minute).WithInformers(
		kubeInformers.InformersFor(operatorclient.GlobalUserSpecifiedConfigNamespace).Core().V1().Secrets().Informer(),
		secretInformer.Informer(),
		operatorClient.Informer(),
	).WithSync(syncer.Sync).ToController("EtcdCertSignerRotationController", c.eventRecorder)
}

func (c *RotationController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	safe, err := c.quorumChecker.IsSafeToUpdateRevision()
	if err != nil {
		return fmt.Errorf("EtcdCertSignerRotationController can't evaluate whether quorum is safe: %w", err)
	}

	if !safe {
		return fmt.Errorf("skipping EtcdCertSignerRotationController reconciliation due to insufficient quorum")
	}

	if err := c.attemptSignerRotation(ctx, syncCtx.Recorder()); err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "EtcdCertSignerRotationControllerDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "Error",
			Message: err.Error(),
		}))
		if updateErr != nil {
			syncCtx.Recorder().Warning("EtcdCertSignerRotationControllerUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(ctx, c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "EtcdCertSignerRotationControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr
}

func (c *RotationController) attemptSignerRotation(ctx context.Context, recorder events.Recorder) error {
	// we detect when the existing openshift-config signer is in its refresh window
	// then we copy the one from openshift-etcd over to openshift-config
	// from there we can await the respective static pod rollouts necessary
	// TODO from here we can also force trigger a renewal via a CRD
	signerCaPair, err := tlshelpers.ReadConfigSignerCert(ctx, c.secretClient)
	if err != nil {
		return err
	}

	// TODO reuse what we have in library-go for the signer certs
	closeEnoughToExpiration := time.Now().After(signerCaPair.Config.Certs[0].NotAfter.Add(-time.Hour * 24 * 3))

	if closeEnoughToExpiration {
		// we need to establish the signer has a long live ahead of itself, otherwise we would need to also
		// TODO delete+await rollout
		newSigner, err := c.secretClient.Secrets(operatorclient.TargetNamespace).Get(ctx, tlshelpers.EtcdSignerCertSecretName, v1.GetOptions{})
		if err != nil {
			return err
		}

		applyConf, err := corev1apply.ExtractSecret(newSigner, "cluster-etcd-operator")
		if err != nil {
			return err
		}

		applyConf = applyConf.WithNamespace(operatorclient.GlobalUserSpecifiedConfigNamespace)
		_, err = c.secretClient.Secrets(operatorclient.GlobalUserSpecifiedConfigNamespace).Apply(ctx, applyConf, v1.ApplyOptions{})
		if err != nil {
			return err
		}

		// TODO await rollout
	}

	return nil
}
