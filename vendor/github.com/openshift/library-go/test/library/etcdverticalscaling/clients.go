package etcdverticalscaling

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"

	machineclient "github.com/openshift/client-go/machine/clientset/versioned"
	machinev1beta1client "github.com/openshift/client-go/machine/clientset/versioned/typed/machine/v1beta1"
	"github.com/openshift/library-go/test/library"
)

func getClients(t TestingT) *ClientSet {
	kubeConfig, err := library.NewClientConfigForTest()
	require.NoError(t, err)
	kubeConfig.Timeout = 60 * time.Second

	machineClientSet, err := machineclient.NewForConfig(kubeConfig)
	require.NoError(t, err)
	machineClient := machineClientSet.MachineV1beta1().Machines("openshift-machine-api")

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err)

	etcdClientFactory := NewEtcdClientFactory(kubeClient)

	return &ClientSet{
		Etcd:    etcdClientFactory,
		Kube:    kubeClient,
		Machine: machineClient,
	}
}

type ClientSet struct {
	Etcd    EtcdClientCreator
	Kube    kubernetes.Interface
	Machine machinev1beta1client.MachineInterface
}

type EtcdClientCreator interface {
	NewEtcdClient() (*clientv3.Client, func(), error)
	NewEtcdClientForMember(memberName string) (*clientv3.Client, func(), error)
}

type EtcdClientFactoryImpl struct {
	kubeClient kubernetes.Interface
}

func NewEtcdClientFactory(kubeClient kubernetes.Interface) *EtcdClientFactoryImpl {
	return &EtcdClientFactoryImpl{kubeClient: kubeClient}
}

func (e *EtcdClientFactoryImpl) NewEtcdClient() (*clientv3.Client, func(), error) {
	return e.newEtcdClientForTarget("service/etcd")
}

func (e *EtcdClientFactoryImpl) NewEtcdClientForMember(memberName string) (*clientv3.Client, func(), error) {
	return e.newEtcdClientForTarget(fmt.Sprintf("pod/etcd-%v", memberName))
}

func (e *EtcdClientFactoryImpl) newEtcdClientForTarget(target string) (*clientv3.Client, func(), error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "oc", "port-forward", target, ":2379", "-n", "openshift-etcd")

	done := func() {
		cancel()
		_ = cmd.Wait() // wait to clean up resources but ignore returned error since cancel kills the process
	}

	var err error // so we can clean up on error
	defer func() {
		if err != nil {
			done()
		}
	}()

	stdOut, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}

	if err = cmd.Start(); err != nil {
		return nil, nil, err
	}

	scanner := bufio.NewScanner(stdOut)
	if !scanner.Scan() {
		return nil, nil, fmt.Errorf("failed to scan port forward std out")
	}
	if err = scanner.Err(); err != nil {
		return nil, nil, err
	}
	output := scanner.Text()

	port := strings.TrimSuffix(strings.TrimPrefix(output, "Forwarding from 127.0.0.1:"), " -> 2379")
	_, err = strconv.Atoi(port)
	if err != nil {
		return nil, nil, fmt.Errorf("port forward output not in expected format: %s", output)
	}

	coreV1 := e.kubeClient.CoreV1()
	etcdConfigMap, err := coreV1.ConfigMaps("openshift-config").Get(ctx, "etcd-ca-bundle", metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	etcdSecret, err := coreV1.Secrets("openshift-config").Get(ctx, "etcd-client", metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}

	tlsConfig, err := restclient.TLSConfigFor(&restclient.Config{
		TLSClientConfig: restclient.TLSClientConfig{
			CertData: etcdSecret.Data[corev1.TLSCertKey],
			KeyData:  etcdSecret.Data[corev1.TLSPrivateKeyKey],
			CAData:   []byte(etcdConfigMap.Data["ca-bundle.crt"]),
		},
	})
	if err != nil {
		return nil, nil, err
	}

	etcdClient3, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"https://127.0.0.1:" + port},
		DialTimeout: 30 * time.Second,
		TLS:         tlsConfig,
	})
	if err != nil {
		return nil, nil, err
	}

	return etcdClient3, done, nil
}
