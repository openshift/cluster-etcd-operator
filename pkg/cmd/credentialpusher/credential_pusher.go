package credentialpusher

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"k8s.io/apimachinery/pkg/util/wait"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/davecgh/go-spew/spew"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/openshift/library-go/pkg/config/client"
	"github.com/openshift/library-go/pkg/operator/events"
)

type CredentialPushOptions struct {
	// TODO replace with genericclioptions
	KubeConfig    string
	KubeClient    kubernetes.Interface
	DynamicClient dynamic.Interface

	CABundleFlags  []string
	TLSSecretFlags []string

	CABundles  []CABundlePushOptions
	TLSSecrets []TLSSecretPushOptions
	NodeName   string

	EventRecorder events.Recorder
}

type CABundlePushOptions struct {
	File      string
	Namespace string
	Name      string
}

type TLSSecretPushOptions struct {
	CertFile  string
	KeyFile   string
	Namespace string
	Name      string
}

func NewCredentialPushOptions() *CredentialPushOptions {
	return &CredentialPushOptions{}
}

func NewCredentialPusher() *cobra.Command {
	o := NewCredentialPushOptions()

	cmd := &cobra.Command{
		Use:   "credential-pusher",
		Short: "Push credentials from files on disk to in-cluster resources",
		Run: func(cmd *cobra.Command, args []string) {
			klog.V(1).Info(cmd.Flags())
			klog.V(1).Info(spew.Sdump(o))

			if err := o.Complete(); err != nil {
				klog.Fatal(err)
			}
			if err := o.Validate(); err != nil {
				klog.Fatal(err)
			}

			if err := o.Run(context.TODO()); err != nil {
				klog.Fatal(err)
			}
		},
	}

	o.AddFlags(cmd.Flags())

	return cmd
}

func (o *CredentialPushOptions) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.KubeConfig, "kubeconfig", o.KubeConfig, "kubeconfig file or empty")
	fs.StringArrayVar(&o.CABundleFlags, "push-ca-bundle", o.CABundleFlags, "repeated in the format '<ca-bundle-file>,<configmap-namespace>,<configmap-name>'")
	fs.StringArrayVar(&o.TLSSecretFlags, "push-tls-secret", o.TLSSecretFlags, "repeated in the format '<cert-file>,<key-file>,<secret-namespace>,<secret-name>'")
}

func (o *CredentialPushOptions) Complete() error {
	clientConfig, err := client.GetKubeConfigOrInClusterConfig(o.KubeConfig, nil)
	if err != nil {
		return err
	}

	// Use protobuf to fetch configmaps and secrets and create pods.
	protoConfig := rest.CopyConfig(clientConfig)
	protoConfig.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	protoConfig.ContentType = "application/vnd.kubernetes.protobuf"

	o.KubeClient, err = kubernetes.NewForConfig(protoConfig)
	if err != nil {
		return err
	}
	o.DynamicClient, err = dynamic.NewForConfig(protoConfig)
	if err != nil {
		return err
	}

	// set via downward API
	o.NodeName = os.Getenv("NODE_NAME")

	for _, curr := range o.CABundleFlags {
		vals := strings.Split(curr, ",")
		if len(vals) != 3 {
			return fmt.Errorf("%q needs to be '<ca-bundle-file>,<configmap-namespace>,<configmap-name>'", curr)
		}
		o.CABundles = append(o.CABundles, CABundlePushOptions{
			File:      vals[0],
			Namespace: vals[1],
			Name:      vals[2],
		})
	}
	for _, curr := range o.TLSSecretFlags {
		vals := strings.Split(curr, ",")
		if len(vals) != 4 {
			return fmt.Errorf("%q needs to be '<cert-file>,<key-file>,<secret-namespace>,<secret-name>'", curr)
		}
		o.TLSSecrets = append(o.TLSSecrets, TLSSecretPushOptions{
			CertFile:  vals[0],
			KeyFile:   vals[1],
			Namespace: vals[2],
			Name:      vals[3],
		})
	}

	eventNamespace := os.Getenv("POD_NAMESPACE")
	o.EventRecorder = events.NewKubeRecorder(o.KubeClient.CoreV1().Events(eventNamespace), "credential-pusher",
		&corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Pod",
			Namespace:  eventNamespace,
			Name:       os.Getenv("POD_NAME"),
		})

	return nil
}

func (o *CredentialPushOptions) Validate() error {
	if len(o.NodeName) == 0 {
		return fmt.Errorf("env var NODE_NAME is required")
	}
	if o.KubeClient == nil {
		return fmt.Errorf("missing client")
	}

	return nil
}

func (o *CredentialPushOptions) pushCABundles(ctx context.Context) error {
	for _, curr := range o.CABundles {
		_, err := o.KubeClient.CoreV1().ConfigMaps(curr.Namespace).Get(curr.Name, metav1.GetOptions{})
		if err == nil {
			klog.V(4).Infof("configmap/%v -n %v is already present", curr.Namespace, curr.Name)
			continue
		}

		caBundle, err := ioutil.ReadFile(curr.File)
		if err != nil {
			o.EventRecorder.Warningf("ca-bundle-error", "%q: error reading: %s", curr.File, err)
			continue
		}
		if len(caBundle) == 0 {
			o.EventRecorder.Warningf("ca-bundle-error", "%q: no content", curr.File)
			continue
		}

		_, err = o.KubeClient.CoreV1().ConfigMaps(curr.Namespace).Create(&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: curr.Namespace, Name: curr.Name},
			Data: map[string]string{
				"ca-bundle.crt": string(caBundle),
			},
		})
		if err != nil {
			o.EventRecorder.Warningf("ca-bundle-error", "%q: error creating: %s", curr.File, err)
			continue
		}
		o.EventRecorder.Eventf("ca-bundle-created", "%q: created as configmap/%v -n %v ", curr.File, curr.Name, curr.Namespace)
	}

	return nil
}

func (o *CredentialPushOptions) pushSecrets(ctx context.Context) error {
	for _, curr := range o.TLSSecrets {
		_, err := o.KubeClient.CoreV1().Secrets(curr.Namespace).Get(curr.Name, metav1.GetOptions{})
		if err == nil {
			klog.V(4).Infof("secret/%v -n %v is already present", curr.Namespace, curr.Name)
			continue
		}

		certFile, err := o.getFileName(ctx, curr.CertFile)
		if err != nil {
			o.EventRecorder.Warningf("tls-secret-error", "%q: error reading: %s", curr.CertFile, err)
			continue
		}
		certContent, err := ioutil.ReadFile(certFile)
		if err != nil {
			o.EventRecorder.Warningf("tls-secret-error", "%q: error reading: %s", certFile, err)
			continue
		}
		if len(certContent) == 0 {
			o.EventRecorder.Warningf("tls-secret-error", "%q: no content", certFile)
			continue
		}

		keyFile, err := o.getFileName(ctx, curr.KeyFile)
		if err != nil {
			o.EventRecorder.Warningf("tls-secret-error", "%q: error reading: %s", curr.CertFile, err)
			continue
		}
		keyContent, err := ioutil.ReadFile(keyFile)
		if err != nil {
			o.EventRecorder.Warningf("tls-secret-error", "%q: error reading: %s", keyFile, err)
			continue
		}
		if len(keyContent) == 0 {
			o.EventRecorder.Warningf("tls-secret-error", "%q: no content", keyFile)
			continue
		}

		_, err = o.KubeClient.CoreV1().Secrets(curr.Namespace).Create(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: curr.Namespace, Name: curr.Name},
			Data: map[string][]byte{
				"tls.crt": certContent,
				"tls.key": keyContent,
			},
		})
		if err != nil {
			o.EventRecorder.Warningf("tls-secret-error", "%q: error creating: %s", certFile, err)
			continue
		}
		o.EventRecorder.Eventf("tls-secret-created", "%q: created as secret/%v -n %v ", certFile, curr.Name, curr.Namespace)
	}

	return nil
}

func (o *CredentialPushOptions) getFileName(ctx context.Context, filename string) (string, error) {
	if !strings.Contains(filename, "ETCD_DNS_NAME_FOR_THIS_NODE") {
		return filename, nil
	}

	dnsName, err := o.getDNSName(o.NodeName)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(filename, "ETCD_DNS_NAME_FOR_THIS_NODE", dnsName), nil
}

func (o *CredentialPushOptions) Run(ctx context.Context) error {
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		if err := o.pushCABundles(ctx); err != nil {
			utilruntime.HandleError(err)
		}
		if err := o.pushSecrets(ctx); err != nil {
			utilruntime.HandleError(err)
		}
	}, 1*time.Minute)

	return nil
}

func (o *CredentialPushOptions) getDNSName(nodeName string) (string, error) {
	controllerConfig, err := o.DynamicClient.
		Resource(schema.GroupVersionResource{Group: "machineconfiguration.openshift.io", Version: "v1", Resource: "controllerconfigs"}).
		Get("machine-config-controller", metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	etcdDiscoveryDomain, ok, err := unstructured.NestedString(controllerConfig.Object, "spec", "etcdDiscoveryDomain")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("controllerconfigs/machine-config-controller missing .spec.etcdDiscoveryDomain")
	}

	ip, err := o.getInternalIPAddressForNodeName(nodeName)
	if err != nil {
		return "", err
	}

	return reverseLookup("etcd-server-ssl", "tcp", etcdDiscoveryDomain, ip)
}

func (o *CredentialPushOptions) getInternalIPAddressForNodeName(nodeName string) (string, error) {
	node, err := o.KubeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	for _, currAddress := range node.Status.Addresses {
		if currAddress.Type == corev1.NodeInternalIP {
			return currAddress.Address, nil
		}
	}
	return "", fmt.Errorf("node/%s missing %s", node.Name, corev1.NodeInternalIP)
}

// returns the target from the SRV record that resolves to ip.
func reverseLookup(service, proto, name, ip string) (string, error) {
	_, srvs, err := net.LookupSRV(service, proto, name)
	if err != nil {
		return "", err
	}
	selfTarget := ""
	for _, srv := range srvs {
		klog.V(4).Infof("checking against %s", srv.Target)
		addrs, err := net.LookupHost(srv.Target)
		if err != nil {
			return "", fmt.Errorf("could not resolve member %q", srv.Target)
		}

		for _, addr := range addrs {
			if addr == ip {
				selfTarget = strings.Trim(srv.Target, ".")
				break
			}
		}
	}
	if selfTarget == "" {
		return "", fmt.Errorf("could not find self")
	}
	return selfTarget, nil
}
