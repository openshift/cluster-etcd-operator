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
	// Name of the secret aggregating certs of this type for all nodes
	allSecretName string
	// Function that derives the name of the cert secret from the node name
	secretNameFunc func(nodeName string) string
	// Function that creates the key material for a new cert
	newCertFunc func(caCert, caKey []byte, ipAddresses []string) (*bytes.Buffer, *bytes.Buffer, error)
}

// Define configuration for creating etcd cert secrets.
var certConfigs = map[string]etcdCertConfig{
	"peer": {
		caSecretName:   "etcd-signer",
		allSecretName:  tlshelpers.EtcdAllPeerSecretName,
		secretNameFunc: tlshelpers.GetPeerClientSecretNameForNode,
		newCertFunc:    tlshelpers.CreatePeerCertKey,
	},
	"serving": {
		caSecretName:   "etcd-signer",
		allSecretName:  tlshelpers.EtcdAllServingSecretName,
		secretNameFunc: tlshelpers.GetServingSecretNameForNode,
		newCertFunc:    tlshelpers.CreateServerCertKey,
	},
	"serving-metrics": {
		caSecretName:   "etcd-metric-signer",
		allSecretName:  tlshelpers.EtcdAllServingMetricsSecretName,
		secretNameFunc: tlshelpers.GetServingMetricsSecretNameForNode,
		newCertFunc:    tlshelpers.CreateMetricCertKey,
	},
}

// Type used to collect all etcd certs during validation e.g.
//
// {
//   "peer":             {"etcd-peer-master-0": []byte{...}, ...},
//   "serving":          {"etcd-serving-master-0": []byte{...}, ...},
//   "serving-metrics":  {"etcd-serving-metrics-master-0": []byte{...}, ...},
// }
type etcdCerts map[string]map[string][]byte

// merge ensures the provided certs are included in the mapping.
func (e etcdCerts) merge(newCerts etcdCerts) {
	for certType, certs := range newCerts {
		// Initialize if necessary
		if _, ok := e[certType]; !ok {
			e[certType] = map[string][]byte{}
		}
		// Copy the new certs. The cert names should be unique by virtue of
		// embedding the node names.
		for name, cert := range certs {
			e[certType][name] = cert
		}
	}
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

	certMap, err := c.ensureCertsForNodes(nodes, recorder)
	if err != nil {
		return err
	}

	// TODO(marun) Ensure static pod controller does not rollout a disjoint
	// set of aggregated certs.
	return c.applyAggregatedCerts(certMap, recorder)
}

// ensureCertsForNodes attempts to ensure the existence of valid cert secrets
// for the provided nodes and if successful returns the validated certs.
func (c *EtcdCertSignerController) ensureCertsForNodes(nodes []*corev1.Node, recorder events.Recorder) (etcdCerts, error) {
	errs := []error{}
	certs := etcdCerts{}
	for _, node := range nodes {
		certsForNode, certErrs := c.ensureCertsForNode(node, recorder)
		if certErrs != nil {
			errs = append(errs, certErrs...)
		}
		if len(errs) > 0 {
			// Any error precludes further cert collection
			continue
		}
		certs.merge(certsForNode)
	}
	if len(errs) > 0 {
		return nil, utilerrors.NewAggregate(errs)
	}
	return certs, nil
}

// applyAggregatedCerts ensures a secret per cert type (e.g. peer, serving,
// metric) containing the provided certs.
func (c *EtcdCertSignerController) applyAggregatedCerts(certMap etcdCerts, recorder events.Recorder) error {
	errs := []error{}
	for certType, secretData := range certMap {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: operatorclient.TargetNamespace,
				Name:      certConfigs[certType].allSecretName,
			},
			Type: corev1.SecretTypeOpaque,
			Data: secretData,
		}
		_, _, err := resourceapply.ApplySecret(c.secretClient, recorder, secret)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return utilerrors.NewAggregate(errs)
}

// ensureCertsForNode attempts to ensure the existence of secrets containing the
// etcd cert (cert+key) pairs needed for an etcd member.
func (c *EtcdCertSignerController) ensureCertsForNode(node *corev1.Node, recorder events.Recorder) (etcdCerts, []error) {
	ipAddresses, err := dnshelpers.GetInternalIPAddressesForNodeName(node)
	if err != nil {
		return nil, []error{err}
	}

	errs := []error{}
	certs := etcdCerts{}
	for certType, certConfig := range certConfigs {
		secretName := certConfig.secretNameFunc(node.Name)
		cert, key, err := c.ensureCertSecret(secretName, string(node.UID), ipAddresses, certConfig, recorder)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		certs[certType] = map[string][]byte{
			fmt.Sprintf("%s.crt", secretName): cert,
			fmt.Sprintf("%s.key", secretName): key,
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return certs, nil
}

// ensureCertSecret attempts to ensure the existence of a secret containing an
// etcd cert (cert+key) pair. The secret will be created if it does not
// exist. If the secret exists but contains an invalid cert pair, it will be
// updated with a new cert pair. If the secret is ensured to have a valid
// cert pair, the bytes of the cert and key will be returned.
func (c *EtcdCertSignerController) ensureCertSecret(secretName, nodeUID string, ipAddresses []string, certConfig etcdCertConfig, recorder events.Recorder) ([]byte, []byte, error) {
	secret, err := c.secretLister.Secrets(operatorclient.TargetNamespace).Get(secretName)
	if err != nil && !errors.IsNotFound(err) {
		return nil, nil, err
	}

	storedUID := ""
	if secret != nil {
		storedUID := secret.Annotations[nodeUIDAnnotation]
		invalidMsg, err := checkCertValidity(secret.Data["tls.crt"], secret.Data["tls.key"], ipAddresses, nodeUID, storedUID)
		if err != nil {
			return nil, nil, err
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
		cert = secret.Data["tls.crt"]
		key = secret.Data["tls.key"]
		if storedUID == nodeUID {
			// Nothing to do: Cert pair is valid, node uid is current
			return cert, key, nil
		}
	} else {
		// Generate a new cert pair. The secret is missing or its contents are invalid.
		caSecret, err := c.secretLister.Secrets(operatorclient.GlobalUserSpecifiedConfigNamespace).Get(certConfig.caSecretName)
		if err != nil {
			return nil, nil, err
		}
		certBuffer, keyBuffer, err := certConfig.newCertFunc(caSecret.Data["tls.crt"], caSecret.Data["tls.key"], ipAddresses)
		if err != nil {
			return nil, nil, err
		}
		cert = certBuffer.Bytes()
		key = keyBuffer.Bytes()
	}

	//TODO: Update annotations Not Before and Not After for Cert Rotation
	newSecret := newCertSecret(secretName, nodeUID, cert, key)
	_, _, err = resourceapply.ApplySecret(c.secretClient, recorder, newSecret)
	if err != nil {
		return nil, nil, err
	}

	return cert, key, nil
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
