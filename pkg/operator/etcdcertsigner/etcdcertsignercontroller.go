package etcdcertsigner

import (
	"bytes"
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
)

type EtcdCertSignerController struct {
	kubeClient           kubernetes.Interface
	operatorClient       v1helpers.OperatorClient
	infrastructureLister configv1listers.InfrastructureLister
	nodeLister           corev1listers.NodeLister
	secretLister         corev1listers.SecretLister
	secretClient         corev1client.SecretsGetter
}

// watches master nodes and maintains secrets for each master node, placing them in a single secret (NOT a tls secret)
// so that the revision controller only has to watch a single secret.  This isn't ideal because it's possible to have a
// revision that is missing the content of a secret, but the actual static pod will fail if that happens and the later
// revision will pick it up.
// This control loop is considerably less robust than the actual cert rotation controller, but I don't have time at the moment
// to make the cert rotation controller dynamic.
func NewEtcdCertSignerController(
	kubeClient kubernetes.Interface,
	operatorClient v1helpers.OperatorClient,

	kubeInformers v1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &EtcdCertSignerController{
		kubeClient:           kubeClient,
		operatorClient:       operatorClient,
		infrastructureLister: infrastructureInformer.Lister(),
		secretLister:         kubeInformers.SecretLister(),
		nodeLister:           kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		secretClient:         v1helpers.CachedSecretGetter(kubeClient.CoreV1(), kubeInformers),
	}
	return factory.New().ResyncEvery(time.Minute).WithInformers(
		kubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Informer(),
		kubeInformers.InformersFor(operatorclient.GlobalUserSpecifiedConfigNamespace).Core().V1().Secrets().Informer(),
		infrastructureInformer.Informer(),
		operatorClient.Informer(),
	).WithSync(c.sync).ToController("EtcdCertSignerController", eventRecorder.WithComponentSuffix("etcd-cert-signer-controller"))
}

func (c *EtcdCertSignerController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	err := c.syncAllMasters(syncCtx.Recorder())
	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
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

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient,
		v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:   "EtcdCertSignerControllerDegraded",
			Status: operatorv1.ConditionFalse,
			Reason: "AsExpected",
		}))
	return updateErr

}

func (c *EtcdCertSignerController) syncAllMasters(recorder events.Recorder) error {
	nodes, err := c.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	if err != nil {
		return err
	}

	errs := []error{}
	for _, node := range nodes {
		if err := c.createSecretForNode(node, recorder); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return utilerrors.NewAggregate(errs)
	}

	// at this point, all the content has been updated in the API, but we may be stale.
	// if we were stale, we would be retriggered on the watch and achieve our level, but the cost of rolling an additional
	// revision on the first go through the API is expensive. Wait for one second to settle most of the time, but still be fast.
	time.Sleep(1 * time.Second)

	// build the combined secrets that we're going to install
	combinedPeerSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorclient.TargetNamespace, Name: tlshelpers.EtcdAllPeerSecretName},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{},
	}
	combinedServingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorclient.TargetNamespace, Name: tlshelpers.EtcdAllServingSecretName},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{},
	}
	combinedServingMetricsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorclient.TargetNamespace, Name: tlshelpers.EtcdAllServingMetricsSecretName},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{},
	}
	for _, node := range nodes {
		peerSecretName := tlshelpers.GetPeerClientSecretNameForNode(node.Name)
		servingSecretName := tlshelpers.GetServingSecretNameForNode(node.Name)
		servingMetricsSecretName := tlshelpers.GetServingMetricsSecretNameForNode(node.Name)

		currPeer, err := c.secretLister.Secrets(operatorclient.TargetNamespace).Get(peerSecretName)
		if err != nil {
			errs = append(errs, err)
		} else {
			combinedPeerSecret.Data[peerSecretName+".crt"] = currPeer.Data["tls.crt"]
			combinedPeerSecret.Data[peerSecretName+".key"] = currPeer.Data["tls.key"]
		}

		currServing, err := c.secretLister.Secrets(operatorclient.TargetNamespace).Get(servingSecretName)
		if err != nil {
			errs = append(errs, err)
		} else {
			combinedServingSecret.Data[servingSecretName+".crt"] = currServing.Data["tls.crt"]
			combinedServingSecret.Data[servingSecretName+".key"] = currServing.Data["tls.key"]
		}

		currServingMetrics, err := c.secretLister.Secrets(operatorclient.TargetNamespace).Get(servingMetricsSecretName)
		if err != nil {
			errs = append(errs, err)
		} else {
			combinedServingMetricsSecret.Data[servingMetricsSecretName+".crt"] = currServingMetrics.Data["tls.crt"]
			combinedServingMetricsSecret.Data[servingMetricsSecretName+".key"] = currServingMetrics.Data["tls.key"]
		}
	}
	if len(errs) > 0 {
		return utilerrors.NewAggregate(errs)
	}

	// apply the secrets themselves
	_, _, err = resourceapply.ApplySecret(c.secretClient, recorder, combinedPeerSecret)
	if err != nil {
		errs = append(errs, err)
	}
	_, _, err = resourceapply.ApplySecret(c.secretClient, recorder, combinedServingSecret)
	if err != nil {
		errs = append(errs, err)
	}
	_, _, err = resourceapply.ApplySecret(c.secretClient, recorder, combinedServingMetricsSecret)
	if err != nil {
		errs = append(errs, err)
	}

	return utilerrors.NewAggregate(errs)
}

func (c *EtcdCertSignerController) createSecretForNode(node *corev1.Node, recorder events.Recorder) error {
	etcdPeerClientCertName := tlshelpers.GetPeerClientSecretNameForNode(node.Name)
	etcdServingCertName := tlshelpers.GetServingSecretNameForNode(node.Name)
	metricsServingCertName := tlshelpers.GetServingMetricsSecretNameForNode(node.Name)

	var err error
	_, err = c.secretLister.Secrets(operatorclient.TargetNamespace).Get(etcdPeerClientCertName)
	peerClientCertOk := err == nil
	_, err = c.secretLister.Secrets(operatorclient.TargetNamespace).Get(etcdServingCertName)
	servingCertOk := err == nil
	_, err = c.secretLister.Secrets(operatorclient.TargetNamespace).Get(metricsServingCertName)
	metricsCertOk := err == nil

	// if we have all the certs we want, do nothing.
	if peerClientCertOk && servingCertOk && metricsCertOk {
		return nil
	}

	// get the signers
	etcdCASecret, err := c.secretLister.Secrets(operatorclient.GlobalUserSpecifiedConfigNamespace).Get("etcd-signer")
	if err != nil {
		return err
	}
	etcdMetricCASecret, err := c.secretLister.Secrets(operatorclient.GlobalUserSpecifiedConfigNamespace).Get("etcd-metric-signer")
	if err != nil {
		return err
	}

	nodeInternalIPs, err := dnshelpers.GetInternalIPAddressesForNodeName(node)
	if err != nil {
		return err
	}

	// create the certificates and update them in the API
	pCert, pKey, err := tlshelpers.CreatePeerCertKey(etcdCASecret.Data["tls.crt"], etcdCASecret.Data["tls.key"], nodeInternalIPs)
	err = c.createSecret(etcdPeerClientCertName, pCert, pKey, recorder)
	if err != nil {
		return err
	}
	sCert, sKey, err := tlshelpers.CreateServerCertKey(etcdCASecret.Data["tls.crt"], etcdCASecret.Data["tls.key"], nodeInternalIPs)
	err = c.createSecret(etcdServingCertName, sCert, sKey, recorder)
	if err != nil {
		return err
	}
	metricCert, metricKey, err := tlshelpers.CreateMetricCertKey(etcdMetricCASecret.Data["tls.crt"], etcdMetricCASecret.Data["tls.key"], nodeInternalIPs)
	err = c.createSecret(metricsServingCertName, metricCert, metricKey, recorder)
	if err != nil {
		return err
	}

	return nil
}

func (c *EtcdCertSignerController) createSecret(secretName string, cert *bytes.Buffer, key *bytes.Buffer, recorder events.Recorder) error {
	//TODO: Update annotations Not Before and Not After for Cert Rotation
	_, _, err := resourceapply.ApplySecret(c.secretClient, recorder, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: operatorclient.TargetNamespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": cert.Bytes(),
			"tls.key": key.Bytes(),
		},
	})
	return err
}
