package etcdcertsigner

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openshift/library-go/pkg/crypto"
	"k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"strings"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/clustermembercontroller"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	corev1client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

const (
	workQueueKey     = "key"
	EtcdCertValidity = 3 * 365 * 24 * time.Hour
	caNamespace      = "openshift-config"
	etcdNamespace    = "openshift-etcd"
	peerOrg          = "system:etcd-peers"
	serverOrg        = "system:etcd-servers"
	metricOrg        = "system:etcd-metrics"
)

type EtcdCertSignerController struct {
	clientset corev1client.Interface
	// Not using this but still keeping it in there
	operatorConfigClient v1helpers.OperatorClient
	queue                workqueue.RateLimitingInterface
	eventRecorder        events.Recorder
}

func NewEtcdCertSignerController(
	clientset corev1client.Interface,
	operatorConfigClient v1helpers.OperatorClient,

	kubeInformersForOpenshiftEtcdNamespace informers.SharedInformerFactory,
	eventRecorder events.Recorder,
) *EtcdCertSignerController {
	c := &EtcdCertSignerController{
		clientset:            clientset,
		operatorConfigClient: operatorConfigClient,
		queue:                workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "EtcdCertSignerController"),
		eventRecorder:        eventRecorder.WithComponentSuffix("etcd-cert-signer-controller"),
	}
	kubeInformersForOpenshiftEtcdNamespace.Core().V1().ConfigMaps().Informer().AddEventHandler(c.eventHandler())
	return c
}

func (c *EtcdCertSignerController) Run(i int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting ClusterMemberController")
	defer klog.Infof("Shutting down ClusterMemberController")

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *EtcdCertSignerController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *EtcdCertSignerController) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

func (c *EtcdCertSignerController) sync() error {
	// TODO: make the namespace and name constants in one of the packages
	cm, err := c.clientset.CoreV1().ConfigMaps(etcdNamespace).Get("member-config", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error getting configmap %#v\n", err)
		return err
	}
	scaling := &clustermembercontroller.EtcdScaling{}
	membershipData, ok := cm.Annotations[clustermembercontroller.EtcdScalingAnnotationKey]
	if !ok {
		// Scaling key not found in configmap, hence do nothing
		return nil
	}
	err = json.Unmarshal([]byte(membershipData), scaling)
	if err != nil {
		klog.Infof("unable to unmarshal scaling data %#v\n", err)
		return err
	}
	//TODO: Add the logic for generating certs
	klog.Infof("Found etcd configmap with data %#v\n", scaling)

	if scaling.Metadata == nil && scaling.Metadata.Name == "" {
		klog.Errorf("unable to get pod name for scaling")
		return errors.New("unable to get pod name")
	}

	pod, err := c.clientset.CoreV1().Pods(etcdNamespace).Get(scaling.Metadata.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pod.Status.HostIP == "" {
		return errors.New("pod does not have host IP assigned")
	}

	etcdCASecret, err := c.clientset.CoreV1().Secrets(caNamespace).Get("etcd-signer", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("unable to get etcd-signer secret %#v", err)
		return err
	}

	etcdMetricCASecret, err := c.clientset.CoreV1().Secrets(caNamespace).Get("etcd-metric-signer", metav1.GetOptions{})
	if err != nil {
		klog.Errorf("unable to get etcd-metric-signer secret %#v", err)
		return err
	}

	err = ensureCASecret(etcdCASecret)
	if err != nil {
		klog.Errorf("etcd-signer ca secret invalid: %v", err)
		return err
	}

	err = ensureCASecret(etcdMetricCASecret)
	if err != nil {
		klog.Errorf("etcd-metric-signer ca secret invalid: %v", err)
		return err
	}

	secretNamespace := pod.Namespace

	peerHostNames := getPeerHostnames(pod, scaling.PodFQDN)

	pCert, pKey, err := getCerts(etcdCASecret.Data["tls.crt"], etcdCASecret.Data["tls.key"], scaling.PodFQDN, peerOrg, peerHostNames)

	err = c.populateSecret(getSecretName(peerOrg, scaling.PodFQDN), secretNamespace, pCert, pKey)
	if err != nil {
		klog.Errorf("unable to create peer secret %#v", err)
		return err
	}

	serverHostNames := getServerHostnames(pod, scaling.PodFQDN)

	sCert, sKey, err := getCerts(etcdCASecret.Data["tls.crt"], etcdCASecret.Data["tls.key"], scaling.PodFQDN, serverOrg, serverHostNames)

	err = c.populateSecret(getSecretName(serverOrg, scaling.PodFQDN), secretNamespace, sCert, sKey)
	if err != nil {
		klog.Errorf("unable to create server secret %#v", err)
		return err
	}

	metricHostNames := getMetricHostnames(pod, scaling.PodFQDN)

	metricCert, metricKey, err := getCerts(etcdMetricCASecret.Data["tls.crt"], etcdMetricCASecret.Data["tls.key"], scaling.PodFQDN, metricOrg, metricHostNames)

	err = c.populateSecret(getSecretName(metricOrg, scaling.PodFQDN), secretNamespace, metricCert, metricKey)
	if err != nil {
		klog.Errorf("unable to create peer secret %#v", err)
		return err
	}

	return nil
}

func getCerts(caCert, caKey []byte, podFQDN, org string, peerHostNames []string) (*bytes.Buffer, *bytes.Buffer, error) {

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

func ensureCASecret(secret *v1.Secret) error {
	if _, ok := secret.Data["tls.crt"]; !ok {
		return errors.New("CA Cert not found")
	}
	if _, ok := secret.Data["tls.key"]; !ok {
		return errors.New("CA Pem not found")
	}
	return nil
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

func getPeerHostnames(pod *v1.Pod, podFQDN string) []string {
	discovery := getDiscoveryDomain(podFQDN)
	ip := pod.Status.HostIP
	return []string{podFQDN, discovery, ip}
}

func getServerHostnames(pod *v1.Pod, podFQDN string) []string {
	return []string{
		"localhost",
		"etcd.kube-system.svc",
		"etcd.kube-system.svc.cluster.local",
		"etcd.openshift-etcd.svc",
		"etcd.openshift-etcd.svc.cluster.local",
		getPodFQDNWildcard(podFQDN),
		pod.Status.HostIP,
		"127.0.0.1",
	}
}

func getMetricHostnames(pod *v1.Pod, podFQDN string) []string {
	return []string{
		"localhost",
		"etcd.kube-system.svc",
		"etcd.kube-system.svc.cluster.local",
		"etcd.openshift-etcd.svc",
		"etcd.openshift-etcd.svc.cluster.local",
		getPodFQDNWildcard(podFQDN),
		pod.Status.HostIP,
	}
}

func getDiscoveryDomain(podFQDN string) string {
	return strings.Join(strings.Split(podFQDN, ".")[1:], ".")
}

func getPodFQDNWildcard(podFQDN string) string {
	return "*." + getDiscoveryDomain(podFQDN)
}

func getSecretName(org, podFQDN string) string {
	if strings.Contains(org, "peer") {
		return "peer-" + podFQDN
	}
	if strings.Contains(org, "server") {
		return "server-" + podFQDN
	}
	if strings.Contains(org, "metric") {
		return "metric-" + podFQDN
	}
	return ""
}

func (c *EtcdCertSignerController) populateSecret(secretName, secretNamespace string, cert *bytes.Buffer, key *bytes.Buffer) error {
	//TODO: Update annotations Not Before and Not After for Cert Rotation
	secret, err := c.clientset.CoreV1().Secrets(secretNamespace).Get(secretName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			secret := &v1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "openshift-etcd"},
				Data: map[string][]byte{
					"tls.crt": cert.Bytes(),
					"tls.key": key.Bytes(),
				},
			}
			_, err := c.clientset.CoreV1().Secrets(secretNamespace).Create(secret)
			return err
		}
		return err
	}
	secret.Data = map[string][]byte{
		"tls.crt": cert.Bytes(),
		"tls.key": key.Bytes(),
	}
	_, err = c.clientset.CoreV1().Secrets(secretNamespace).Update(secret)
	return err
}

// eventHandler queues the operator to check spec and status
func (c *EtcdCertSignerController) eventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
