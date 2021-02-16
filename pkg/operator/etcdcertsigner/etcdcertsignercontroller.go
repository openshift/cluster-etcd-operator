package etcdcertsigner

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
)

// Annotation key used to associate a cert secret with a node uid. This allows
// cert regeneration if a node was deleted and added with the same name.
const nodeUIDAnnotation = "etcd-operator.alpha.openshift.io/cert-secret-node-uid"

// etcdCertConfig defines the configuration required to maintain a cert secret for an etcd member.
type etcdCertConfig struct {
	// Name of the secret in namespace openshift-config that contains the CA used to issue the cert
	caSecretName string
	// Function that derives the name of the cert secret from the node name
	secretNameFunc func(nodeName string) string
	// Function that creates the key material for a new cert
	newCertFunc func(caCert, caKey []byte, ipAddresses []string) (*bytes.Buffer, *bytes.Buffer, error)
}

// Define configuration for creating etcd cert secrets.
var certConfigMap = map[string]etcdCertConfig{
	"peer": {
		caSecretName:   "etcd-signer",
		secretNameFunc: tlshelpers.GetPeerClientSecretNameForNode,
		newCertFunc:    tlshelpers.CreatePeerCertKey,
	},
	"server": {
		caSecretName:   "etcd-signer",
		secretNameFunc: tlshelpers.GetServingSecretNameForNode,
		newCertFunc:    tlshelpers.CreateServerCertKey,
	},
	"metric": {
		caSecretName:   "etcd-metric-signer",
		secretNameFunc: tlshelpers.GetServingMetricsSecretNameForNode,
		newCertFunc:    tlshelpers.CreateMetricCertKey,
	},
}

type EtcdCertSignerController struct {
	kubeClient     kubernetes.Interface
	operatorClient v1helpers.OperatorClient
	nodeLister     corev1listers.NodeLister
	secretLister   corev1listers.SecretLister
	secretClient   corev1client.SecretsGetter
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
	eventRecorder events.Recorder,
) factory.Controller {
	c := &EtcdCertSignerController{
		kubeClient:     kubeClient,
		operatorClient: operatorClient,
		secretLister:   kubeInformers.SecretLister(),
		nodeLister:     kubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		secretClient:   v1helpers.CachedSecretGetter(kubeClient.CoreV1(), kubeInformers),
	}
	return factory.New().ResyncEvery(time.Minute).WithInformers(
		kubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
		kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets().Informer(),
		kubeInformers.InformersFor(operatorclient.GlobalUserSpecifiedConfigNamespace).Core().V1().Secrets().Informer(),
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
		certErrs := c.ensureCertSecrets(node, recorder)
		if certErrs != nil {
			errs = append(errs, certErrs...)
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

// ensureCertSecret attempts to ensure the existence of secrets containing the
// etcd cert (cert+key) pairs needed for an etcd member.
func (c *EtcdCertSignerController) ensureCertSecrets(node *corev1.Node, recorder events.Recorder) []error {
	ipAddresses, err := dnshelpers.GetInternalIPAddressesForNodeName(node)
	if err != nil {
		return []error{err}
	}

	errs := []error{}
	for _, certConfig := range certConfigMap {
		err := c.ensureCertSecret(node, ipAddresses, certConfig, recorder)
		if err != nil {
			errs = append(errs, err)
			continue
		}
	}
	return errs
}

// ensureCertSecret attempts to ensure the existence of a secret containing an
// etcd cert (cert+key) pair. The secret will be created if it does not
// exist. If the secret exists but contains an invalid cert pair, it will be
// updated with a new cert pair.
func (c *EtcdCertSignerController) ensureCertSecret(node *corev1.Node, ipAddresses []string, certConfig etcdCertConfig, recorder events.Recorder) error {
	secretName := certConfig.secretNameFunc(node.Name)

	secret, err := c.secretLister.Secrets(operatorclient.TargetNamespace).Get(secretName)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	storedUID := ""
	if secret != nil {
		storedUID := secret.Annotations[nodeUIDAnnotation]
		invalidMsg, err := checkCertValidity(secret.Data["tls.crt"], secret.Data["tls.key"], ipAddresses, string(node.UID), storedUID)
		if err != nil {
			return err
		}
		if len(invalidMsg) > 0 {
			klog.V(4).Info("TLS cert %s is invalid and will be regenerated: %v", secretName, invalidMsg)
			// A nil secret will prompt creation of a new keypair
			secret = nil
		}
	}

	cert := []byte{}
	key := []byte{}
	if secret != nil {
		if storedUID == string(node.UID) {
			// Nothing to do: Cert pair is valid, node uid is current
			return nil
		}
		cert = secret.Data["tls.crt"]
		key = secret.Data["tls.key"]
	} else {
		// Generate a new cert pair. The secret is missing or its contents are invalid.
		caSecret, err := c.secretLister.Secrets(operatorclient.GlobalUserSpecifiedConfigNamespace).Get(certConfig.caSecretName)
		if err != nil {
			return err
		}
		certBuffer, keyBuffer, err := certConfig.newCertFunc(caSecret.Data["tls.crt"], caSecret.Data["tls.key"], ipAddresses)
		if err != nil {
			return err
		}
		cert = certBuffer.Bytes()
		key = keyBuffer.Bytes()
	}

	return c.applySecret(secretName, string(node.UID), cert, key, recorder)
}

func (c *EtcdCertSignerController) applySecret(secretName, nodeUID string, cert, key []byte, recorder events.Recorder) error {
	//TODO: Update annotations Not Before and Not After for Cert Rotation
	secret := newCertSecret(secretName, nodeUID, cert, key)
	_, _, err := resourceapply.ApplySecret(c.secretClient, recorder, secret)
	return err
}

// newCertSecret ensures consistency of secret creation between the controller
// and its tests.
func newCertSecret(secretName, nodeUID string, cert, key []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: operatorclient.TargetNamespace,
			Annotations: map[string]string{
				nodeUIDAnnotation: nodeUID,
			},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			"tls.crt": cert,
			"tls.key": key,
		},
	}
}

// checkCertValidity validates the provided cert bytes. If the cert needs to be
// regenerated, a message will be returned indicating why. An empty message
// indicates a valid cert. An error will be returned if the cert is not valid
// and should not be regenerated.
func checkCertValidity(certBytes, keyBytes []byte, ipAddresses []string, nodeUID, storedUID string) (string, error) {
	// Loading the keypair without error indicates the key material is valid and
	// the cert and private key are related.
	keyPair, err := tls.X509KeyPair(certBytes, keyBytes)
	if err != nil {
		// Invalid keypair - regen required
		return fmt.Sprintf("%v", err), nil
	}
	leafCert, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		// Should never happen once x509KeyPair() succeeds
		return fmt.Sprintf("%v", err), nil
	}

	// Check that the cert is valid for the provided ip addresses
	errs := []error{}
	for _, ipAddress := range ipAddresses {
		if err := leafCert.VerifyHostname(ipAddress); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		// When a cluster is upgraded to 4.8 - the first release to annotate
		// cert secrets with the node UID - previously created secrets will
		// initially not have a node UID set. Assume that the lack of a stored
		// node UID indicates that the certs were generated for the current node
		// so that if the cert is not valid for the node's current ip addresses
		// it will be flagged as an error.
		//
		// TODO(marun) The check for a missing stored uid can be removed in 4.9
		// since by then all cert secrets will be guaranteed to have a stored
		// node uid. That would change the conditional from ternary to binary
		// and allow the use of a boolean indication of node uid change.
		nodeUIDUnchanged := nodeUID == storedUID || len(storedUID) == 0

		if nodeUIDUnchanged {
			// This is an error condition. If the cert SAN doesn't include all
			// of ip's for the node that it was generated for, an out-of-band
			// etcd cluster membership change may be required. The operator
			// doesn't currently handle this.
			return "", utilerrors.NewAggregate(errs)
		} else {
			// A different node uid indicates node removal and addition with the
			// same name. etcd on the node will be a new cluster member and
			// needs a new certificate.
			msgs := []string{}
			for _, err := range errs {
				msgs = append(msgs, fmt.Sprintf("%v", err))
			}
			return strings.Join(msgs, ", "), nil
		}
	}

	// TODO(marun) Check that the certificate was issued by the CA

	// TODO(marun) Check that the certificate is not expired or due for replacement

	// Cert is valid
	return "", nil
}
