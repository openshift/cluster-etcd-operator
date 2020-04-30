package etcdcertsigner

import (
	"bytes"
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	EtcdCertValidity = 10 * 365 * 24 * time.Hour
	peerOrg          = "system:etcd-peers"
	serverOrg        = "system:etcd-servers"
	metricOrg        = "system:etcd-metrics"
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
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorclient.TargetNamespace, Name: "etcd-all-peer"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{},
	}
	combinedServingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorclient.TargetNamespace, Name: "etcd-all-serving"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{},
	}
	combinedServingMetricsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: operatorclient.TargetNamespace, Name: "etcd-all-serving-metrics"},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{},
	}
	for _, node := range nodes {
		currPeer, err := c.secretLister.Secrets(operatorclient.TargetNamespace).Get(getPeerClientSecretNameForNode(node))
		if err != nil {
			errs = append(errs, err)
		} else {
			combinedPeerSecret.Data[getPeerClientSecretNameForNode(node)+".crt"] = currPeer.Data["tls.crt"]
			combinedPeerSecret.Data[getPeerClientSecretNameForNode(node)+".key"] = currPeer.Data["tls.key"]
		}

		currServing, err := c.secretLister.Secrets(operatorclient.TargetNamespace).Get(getServingSecretNameForNode(node))
		if err != nil {
			errs = append(errs, err)
		} else {
			combinedServingSecret.Data[getServingSecretNameForNode(node)+".crt"] = currServing.Data["tls.crt"]
			combinedServingSecret.Data[getServingSecretNameForNode(node)+".key"] = currServing.Data["tls.key"]
		}

		currServingMetrics, err := c.secretLister.Secrets(operatorclient.TargetNamespace).Get(getServingMetricsSecretNameForNode(node))
		if err != nil {
			errs = append(errs, err)
		} else {
			combinedServingMetricsSecret.Data[getServingMetricsSecretNameForNode(node)+".crt"] = currServingMetrics.Data["tls.crt"]
			combinedServingMetricsSecret.Data[getServingMetricsSecretNameForNode(node)+".key"] = currServingMetrics.Data["tls.key"]
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

func getPeerClientSecretNameForNode(node *corev1.Node) string {
	return fmt.Sprintf("etcd-peer-%s", node.Name)
}
func getServingSecretNameForNode(node *corev1.Node) string {
	return fmt.Sprintf("etcd-serving-%s", node.Name)
}
func getServingMetricsSecretNameForNode(node *corev1.Node) string {
	return fmt.Sprintf("etcd-serving-metrics-%s", node.Name)
}

func (c *EtcdCertSignerController) createSecretForNode(node *corev1.Node, recorder events.Recorder) error {
	etcdPeerClientCertName := getPeerClientSecretNameForNode(node)
	etcdServingCertName := getServingSecretNameForNode(node)
	metricsServingCertName := getServingMetricsSecretNameForNode(node)

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

	// get what we're going to sign for
	etcdDiscoveryDomain, err := c.getEtcdDiscoveryDomain()
	if err != nil {
		return err
	}
	nodeInternalIPs, err := dnshelpers.GetInternalIPAddressesForNodeName(node)
	if err != nil {
		return err
	}
	peerHostNames := append([]string{"localhost", etcdDiscoveryDomain}, nodeInternalIPs...)
	serverHostNames := append([]string{
		"localhost",
		"etcd.kube-system.svc",
		"etcd.kube-system.svc.cluster.local",
		"etcd.openshift-etcd.svc",
		"etcd.openshift-etcd.svc.cluster.local",
		"*." + etcdDiscoveryDomain,
		"127.0.0.1",
		"::1",
		"0:0:0:0:0:0:0:1",
	}, nodeInternalIPs...)
	// TODO debt left for @hexfusion or @sanchezl
	fakePodFQDN := "etcd-client"

	// create the certificates and update them in the API
	pCert, pKey, err := createNewCombinedClientAndServingCerts(etcdCASecret.Data["tls.crt"], etcdCASecret.Data["tls.key"], fakePodFQDN, peerOrg, peerHostNames)
	err = c.createSecret(etcdPeerClientCertName, pCert, pKey, recorder)
	if err != nil {
		return err
	}
	sCert, sKey, err := createNewCombinedClientAndServingCerts(etcdCASecret.Data["tls.crt"], etcdCASecret.Data["tls.key"], fakePodFQDN, serverOrg, serverHostNames)
	err = c.createSecret(etcdServingCertName, sCert, sKey, recorder)
	if err != nil {
		return err
	}
	metricCert, metricKey, err := createNewCombinedClientAndServingCerts(etcdMetricCASecret.Data["tls.crt"], etcdMetricCASecret.Data["tls.key"], fakePodFQDN, metricOrg, serverHostNames)
	err = c.createSecret(metricsServingCertName, metricCert, metricKey, recorder)
	if err != nil {
		return err
	}

	return nil
}

func createNewCombinedClientAndServingCerts(caCert, caKey []byte, podFQDN, org string, peerHostNames []string) (*bytes.Buffer, *bytes.Buffer, error) {
	cn, err := getCommonNameFromOrg(org)
	etcdCAKeyPair, err := crypto.GetCAFromBytes(caCert, caKey)
	if err != nil {
		return nil, nil, err
	}

	certConfig, err := etcdCAKeyPair.MakeServerCertForDuration(sets.NewString(peerHostNames...), EtcdCertValidity, func(cert *x509.Certificate) error {

		cert.Issuer = pkix.Name{
			OrganizationalUnit: []string{"openshift"},
			CommonName:         cn,
		}
		cert.Subject = pkix.Name{
			Organization: []string{org},
			CommonName:   strings.TrimSuffix(org, "s") + ":" + podFQDN,
		}
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}

		// TODO: Extended Key Usage:
		// All profiles expect a x509.ExtKeyUsageCodeSigning set on extended Key Usages
		// need to investigage: https://github.com/etcd-io/etcd/issues/9398#issuecomment-435340312
		// TODO: some extensions are missing form cfssl.
		// e.g.
		//	X509v3 Subject Key Identifier:
		//		B7:30:0B:CF:47:4E:21:AE:13:60:74:42:B0:D9:C4:F3:26:69:63:03
		//	X509v3 Authority Key Identifier:
		//		keyid:9B:C0:6B:0C:8E:5C:73:6A:83:B1:E4:54:97:D3:62:18:8A:9C:BC:1E
		// TODO: Change serial number logic, to something as follows.
		// The following is taken from CFSSL library.
		// If CFSSL is providing the serial numbers, it makes
		// sense to use the max supported size.

		//	serialNumber := make([]byte, 20)
		//	_, err = io.ReadFull(rand.Reader, serialNumber)
		//	if err != nil {
		//		return err
		//	}
		//
		//	// SetBytes interprets buf as the bytes of a big-endian
		//	// unsigned integer. The leading byte should be masked
		//	// off to ensure it isn't negative.
		//	serialNumber[0] &= 0x7F
		//	cert.SerialNumber = new(big.Int).SetBytes(serialNumber)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	certBytes := &bytes.Buffer{}
	keyBytes := &bytes.Buffer{}
	if err := certConfig.WriteCertConfig(certBytes, keyBytes); err != nil {
		return nil, nil, err
	}
	return certBytes, keyBytes, nil
}

func getCommonNameFromOrg(org string) (string, error) {
	if strings.Contains(org, "peer") || strings.Contains(org, "server") {
		return "etcd-signer", nil
	}
	if strings.Contains(org, "metric") {
		return "etcd-metric-signer", nil
	}
	return "", errors.New("unable to recognise secret name")
}

func (c *EtcdCertSignerController) getEtcdDiscoveryDomain() (string, error) {
	infrastructure, err := c.infrastructureLister.Get("cluster")
	if err != nil {
		return "", err
	}

	etcdDiscoveryDomain := infrastructure.Status.EtcdDiscoveryDomain
	if len(etcdDiscoveryDomain) == 0 {
		return "", fmt.Errorf("infrastructures.config.openshit.io/cluster missing .status.etcdDiscoveryDomain")
	}
	return etcdDiscoveryDomain, nil
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
