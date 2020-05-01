package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	test "github.com/openshift/cluster-etcd-operator/test/library"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/events/eventstesting"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	"go.etcd.io/etcd/pkg/transport"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
)

type TestFixture struct {
	dir    string
	caCert string
	key    string
	cert   string
}

type PortForwardAPodRequest struct {
	// RestConfig is the kubernetes config
	RestConfig *rest.Config
	// Pod is the selected pod for this port forwarding
	Pod v1.Pod
	// LocalPort is the local port that will be selected to expose the PodPort
	LocalPort int
	// PodPort is the target port for the pod
	PodPort int
	// Steams configures where to write or read input from
	Streams genericclioptions.IOStreams
	// StopCh is the channel used to manage the port forward lifecycle
	StopCh <-chan struct{}
	// ReadyCh communicates when the tunnel is ready to receive traffic
	ReadyCh chan struct{}
}

func TestOperatorNamespace(t *testing.T) {
	kubeConfig, err := test.NewClientConfigForTest()
	require.NoError(t, err)
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err)
	_, err = kubeClient.CoreV1().Namespaces().Get(context.TODO(), "openshift-etcd-operator", metav1.GetOptions{})
	require.NoError(t, err)
}

func TestEndpointStatus(t *testing.T) {
	kubeConfig, err := test.NewClientConfigForTest()
	require.NoError(t, err)
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err)

	cleanupFixtures, fixtures := createTestFixtures(t)
	defer cleanupFixtures()

	cleanupPortForward := createPortForward(t)
	defer cleanupPortForward()

	configClient, err := configv1client.NewForConfig(kubeConfig)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kubeInformersForNamespaces := v1helpers.NewKubeInformersForNamespaces(
		kubeClient,
		"",
		operatorclient.GlobalUserSpecifiedConfigNamespace,
		operatorclient.GlobalMachineSpecifiedConfigNamespace,
		operatorclient.TargetNamespace,
		operatorclient.OperatorNamespace,
		"kube-system",
		"openshift-machine-config-operator", // TODO remove after quorum-guard is removed from MCO
	)

	tlsInfo := transport.TLSInfo{
		CertFile:      fixtures.cert,
		KeyFile:       fixtures.key,
		TrustedCAFile: fixtures.caCert,
	}

	t.Logf("### TLS %v", tlsInfo)

	realEventRecorder := events.NewRecorder(kubeClient.CoreV1().Events("openshift-etcd-operator"), "test-etcdClient", &v1.ObjectReference{})
	eventRecorder := eventstesting.NewEventRecorder(t, realEventRecorder)

	configInformers := configv1informers.NewSharedInformerFactory(configClient, 10*time.Minute)
	etcdClient := etcdcli.NewEtcdClient(
		kubeInformersForNamespaces,
		configInformers.Config().V1().Networks(),
		tlsInfo,
		[]string{"https://127.0.0.1:8080", "https://127.0.0.1:8081", "https://127.0.0.1:8082"},
		eventRecorder)

	// start informers
	kubeInformersForNamespaces.Start(ctx.Done())
	configInformers.Start(ctx.Done())

	// allow to sync yes we can be smarter.
	time.Sleep(120 * time.Second)

	etcdMembers := []*etcdserverpb.Member{
		{Name: "etcd-1", ClientURLs: []string{"https://127.0.0.1:8080"}},
		{Name: "etcd-2", ClientURLs: []string{"https://127.0.0.1:8081"}},
		{Name: "etcd-3", ClientURLs: []string{"https://127.0.0.1:8082"}},
	}

	availableMembers, unstartedMembers, unhealthyMembers := []string{}, []string{}, []string{}

	for _, m := range etcdMembers {
		switch etcdClient.MemberStatus(m) {
		case etcdcli.EtcdMemberStatusAvailable:
			availableMembers = append(availableMembers, m.Name)
		case etcdcli.EtcdMemberStatusNotStarted:
			unstartedMembers = append(unstartedMembers, etcdcli.GetMemberNameOrHost(m))
		case etcdcli.EtcdMemberStatusUnhealthy:
			unhealthyMembers = append(unhealthyMembers, m.Name)
		}
	}
	t.Logf("unhealthy %v\n", unhealthyMembers)
	t.Logf("healthy %v\n", availableMembers)
	t.Logf("unstarted %v\n", unstartedMembers)

}

// This opens up a port forward between the test host and the container allowing test etcdcli calls to execute correctly.
func createPortForward(t *testing.T) func() {
	kubeConfig, err := test.NewClientConfigForTest()
	require.NoError(t, err)
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err)
	podList, err := kubeClient.CoreV1().Pods("openshift-etcd").List(context.TODO(), metav1.ListOptions{LabelSelector: "app=etcd"})
	require.NoError(t, err)
	var wg sync.WaitGroup

	for i := 0; i <= 2; {
		wg.Add(1)
		readyCh := make(chan struct{})
		stopCh := make(chan struct{})
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigs
			close(stopCh)
			wg.Done()
		}()

		stream := genericclioptions.IOStreams{
			In:     os.Stdin,
			Out:    os.Stdout,
			ErrOut: os.Stderr,
		}

		go func() {
			err := portForwardAPod(PortForwardAPodRequest{
				RestConfig: kubeConfig,
				Pod: v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podList.Items[i].Name,
						Namespace: podList.Items[0].Namespace,
					},
				},
				LocalPort: 8080 + i,
				PodPort:   2379,
				Streams:   stream,
				StopCh:    stopCh,
				ReadyCh:   readyCh,
			})
			if err != nil {
				panic(err)
			}
		}()
		select {
		case <-readyCh:
			i++
			continue
		}

	}

	return func() {
		// cleanup
		wg.Done()
	}

}

func createTestFixtures(t *testing.T) (func(), *TestFixture) {
	kubeConfig, err := test.NewClientConfigForTest()
	require.NoError(t, err)
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err)

	podList, err := kubeClient.CoreV1().Pods("openshift-etcd").List(context.TODO(), metav1.ListOptions{LabelSelector: "app=etcd"})
	require.NoError(t, err)

	stdout, _, _ := executeRemoteCommand(&podList.Items[0], "/bin/sh", "-c", "cat $ETCDCTL_CACERT | base64 -w 0; echo && cat $ETCDCTL_KEY | base64 -w 0; echo && cat $ETCDCTL_CERT | base64 -w 0; echo")
	require.NoError(t, err)
	// t.Logf("out: %v", stdout)

	dir, err := ioutil.TempDir("/tmp", "fixtures-")
	if err != nil {
		t.Fatal(err)
	}

	fixtureData := strings.Split(stdout, "\n")

	// ca
	caCertData, err := base64.StdEncoding.DecodeString(fixtureData[0])
	require.NoError(t, err)

	caCertFile, err := ioutil.TempFile(dir, "ca-*.crt")
	if err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(caCertFile.Name(), caCertData, 0644); err != nil {
		t.Fatal(err)
	}
	caCertFile.Close()

	// key
	keyData, err := base64.StdEncoding.DecodeString(fixtureData[1])
	require.NoError(t, err)
	keyFile, err := ioutil.TempFile(dir, "key-*.crt")
	if err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(keyFile.Name(), keyData, 0644); err != nil {
		t.Fatal(err)
	}
	keyFile.Close()

	// cert
	certData, err := base64.StdEncoding.DecodeString(fixtureData[2])
	require.NoError(t, err)
	certFile, err := ioutil.TempFile(dir, "cert-*.crt")
	if err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(certFile.Name(), certData, 0644); err != nil {
		t.Fatal(err)
	}
	certFile.Close()

	return func() {
			// cleanup
			os.RemoveAll(dir)
		}, &TestFixture{
			dir:    dir,
			caCert: caCertFile.Name(),
			key:    keyFile.Name(),
			cert:   certFile.Name(),
		}

}

func portForwardAPod(req PortForwardAPodRequest) error {
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward",
		req.Pod.Namespace, req.Pod.Name)
	hostIP := strings.TrimLeft(req.RestConfig.Host, "htps:/")

	transport, upgrader, err := spdy.RoundTripperFor(req.RestConfig)
	if err != nil {
		return err
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, &url.URL{Scheme: "https", Path: path, Host: hostIP})
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("%d:%d", req.LocalPort, req.PodPort)}, req.StopCh, req.ReadyCh, req.Streams.Out, req.Streams.ErrOut)
	if err != nil {
		return err
	}
	return fw.ForwardPorts()
}

func executeRemoteCommand(pod *v1.Pod, cmd ...string) (string, string, error) {
	kubeConfig, err := test.NewClientConfigForTest()
	if err != nil {
		return "", "", err
	}
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return "", "", err
	}

	req := kubeClient.CoreV1().RESTClient().Post().
		Namespace(pod.Namespace).
		Resource("pods").
		Name(pod.Name).
		SubResource("exec")
	scheme := runtime.NewScheme()
	if err := v1.AddToScheme(scheme); err != nil {
		return "", "", fmt.Errorf("error adding to scheme: %v", err)
	}
	parameterCodec := runtime.NewParameterCodec(scheme)

	req.VersionedParams(&v1.PodExecOptions{
		Command:   cmd,
		Container: pod.Spec.Containers[0].Name,
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, parameterCodec)

	fmt.Printf("url: %v", req.URL())
	exec, err := remotecommand.NewSPDYExecutor(kubeConfig, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("error while creating Executor: %v", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    true,
	})
	if err != nil {
		return "", "", errors.Wrapf(err, "Failed executing command %s on %v/%v -c %s", cmd, pod.Namespace, pod.Name, pod.Spec.Containers[0].Name)
	}

	return strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()), nil
}

func TestRevisionLimits(t *testing.T) {
	kubeConfig, err := test.NewClientConfigForTest()
	require.NoError(t, err)
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err)
	operatorClient, _, err := genericoperatorclient.NewStaticPodOperatorClient(kubeConfig, operatorv1.GroupVersion.WithResource("etcds"))
	require.NoError(t, err)

	// Get current revision limits
	operatorSpec, _, _, err := operatorClient.GetStaticPodOperatorStateWithQuorum()
	require.NoError(t, err)

	totalRevisionLimit := operatorSpec.SucceededRevisionLimit + operatorSpec.FailedRevisionLimit
	if operatorSpec.SucceededRevisionLimit == 0 {
		totalRevisionLimit += 2
	}
	if operatorSpec.FailedRevisionLimit == 0 {
		totalRevisionLimit += 2
	}

	revisions, err := getRevisionStatuses(kubeClient)
	require.NoError(t, err)

	// Check if revisions are being quickly created to test for operator hotlooping
	changes := 0
	pollsWithoutChanges := 0
	lastRevisionCount := len(revisions)
	err = wait.PollImmediate(1*time.Second, 10*time.Minute, func() (bool, error) {
		newRevisions, err := getRevisionStatuses(kubeClient)
		require.NoError(t, err)

		// If there are more revisions than the total allowed Failed and Succeeded revisions, then there must be some that
		// are InProgress or Unknown (since these do not count toward failed or succeeded), which could indicate zombie revisions.
		// Check total+1 to account for possibly a current new revision that just hasn't pruned off the oldest one yet.
		if len(newRevisions) > int(totalRevisionLimit)+1 {
			// TODO(marun) If number of revisions has been exceeded, need to give time for the pruning controller to
			// progress rather than immediately failing.
			// t.Errorf("more revisions (%v) than total allowed (%v): %+v", len(revisions), totalRevisionLimit, revisions)
		}

		// No revisions in the last 30 seconds probably means we're not rapidly creating new ones and can return
		if len(newRevisions)-lastRevisionCount == 0 {
			pollsWithoutChanges += 1
			if pollsWithoutChanges >= 30 {
				return true, nil
			}
		} else {
			pollsWithoutChanges = 0
		}
		changes += len(newRevisions) - lastRevisionCount
		if changes >= 20 {
			return true, fmt.Errorf("too many new revisions created, possible hotlooping detected")
		}
		lastRevisionCount = len(newRevisions)
		return false, nil
	})
	require.NoError(t, err)
}

func getRevisionStatuses(kubeClient *kubernetes.Clientset) (map[string]string, error) {
	configMaps, err := kubeClient.CoreV1().ConfigMaps(operatorclient.TargetNamespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return map[string]string{}, err
	}
	revisions := map[string]string{}
	for _, configMap := range configMaps.Items {
		if !strings.HasPrefix(configMap.Name, "revision-status-") {
			continue
		}
		if revision, ok := configMap.Data["revision"]; ok {
			revisions[revision] = configMap.Data["status"]
		}
	}
	return revisions, nil
}
