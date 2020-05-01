package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	test "github.com/openshift/cluster-etcd-operator/test/library"
	"github.com/openshift/library-go/pkg/operator/genericoperatorclient"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
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

func portForwarder(pod *v1.Pod, kubeConfig *rest.Config, ready chan<- struct{}, seq int) {
	fmt.Printf("hello %v", ready)
	readyCh := make(chan struct{})
	stopCh := make(chan struct{})
	stream := genericclioptions.IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}
	err := PortForwardAPod(PortForwardAPodRequest{
		RestConfig: kubeConfig,
		Pod: v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
		},
		LocalPort: 8080 + seq,
		PodPort:   2379,
		Streams:   stream,
		StopCh:    stopCh,
		ReadyCh:   readyCh,
	})
	if err != nil {
		println("ooo shit")
		panic(err)
	}
	select {
	case <-readyCh:
		ready <- struct{}{}
		break
	}

}

// This opens up a port forward between the test host and the container allowing test etcdcli calls to execute correctly.
func TestPortForward(t *testing.T) {
	kubeConfig, err := test.NewClientConfigForTest()
	require.NoError(t, err)
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err)
	podList, err := kubeClient.CoreV1().Pods("openshift-etcd").List(context.TODO(), metav1.ListOptions{LabelSelector: "app=etcd"})
	require.NoError(t, err)
	var wg sync.WaitGroup
	// stopCh := make(chan struct{})
	//
	// // stopCh control the port forwarding lifecycle. When it gets closed the
	// // port forward will terminate
	// stopCh := make(chan struct{}, 1)
	// // readyCh communicate when the port forward is ready to get traffic
	// readyCh := make(chan struct{})
	// // stream is used to tell the port forwarder where to place its output or
	// // where to expect input if needed. For the port forwarding we just need
	// // the output eventually
	// stream := genericclioptions.IOStreams{
	// 	In:     os.Stdin,
	// 	Out:    os.Stdout,
	// 	ErrOut: os.Stderr,
	// }
	//
	// // managing termination signal from the terminal. As you can see the stopCh
	// // gets closed to gracefully handle its termination.
	// sigs := make(chan os.Signal, 1)
	// signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	// go func() {
	// 	<-sigs
	// 	fmt.Println("Bye...")
	// 	close(stopCh)
	// 	wg.Done()
	// }()
	//
	// go func() {
	//
	// 	t.Logf("adding new portforwarder %d", i)
	// 	stream := genericclioptions.IOStreams{
	// 		In:     os.Stdin,
	// 		Out:    os.Stdout,
	// 		ErrOut: os.Stderr,
	// 	}
	// 	err := PortForwardAPod(PortForwardAPodRequest{
	// 		RestConfig: kubeConfig,
	// 		Pod: v1.Pod{
	// 			ObjectMeta: metav1.ObjectMeta{
	// 				Name:      podList.Items[i].Name,
	// 				Namespace: podList.Items[0].Namespace,
	// 			},
	// 		},
	// 		LocalPort: 8080 + i,
	// 		PodPort:   2379,
	// 		Streams:   stream,
	// 		StopCh:    stopCh,
	// 		ReadyCh:   readyCh,
	// 	})
	// 	if err != nil {
	// 		println("ooo shit")
	// 		panic(err)
	// 	}
	// }()
	//
	// select {
	// case <-readyCh:
	// 	break
	// }
	// for i := 0; i <= 2; i++ {
	// 	// go portForwarder(&podList.Items[i], kubeConfig, ready, i)
	//
	// 	t.Logf("sent job %d", i)
	// }
	//
	// for readyCount := 0; readyCount < 2; readyCount++ {
	// 	<-ready
	// 	t.Logf("ready %d", readyCount)
	// }
	//
	// wg.Add(1)
	// wg.Add(1)
	// stopCh := make(chan struct{})
	// readyCh := make(chan struct{})
	// errCh := make(chan error)
	// defer func() {
	// 	close(stopCh)
	//
	// 	forwardErr := <-errCh
	// 	if forwardErr != nil {
	// 		t.Fatalf("ForwardPorts returned error: %s", forwardErr)
	// 	}
	// 	wg.Done()
	// }()
	//

	for i := 0; i <= 2; {

		wg.Add(1)
		// sigs := make(chan os.Signal, 1)
		// signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		// go func() {
		// 	<-sigs
		// 	fmt.Println("Bye...")
		// 	close(stopCh)
		// 	wg.Done()
		// }()
		readyCh := make(chan struct{})

		stopCh := make(chan struct{})
		stream := genericclioptions.IOStreams{
			In:     os.Stdin,
			Out:    os.Stdout,
			ErrOut: os.Stderr,
		}

		fmt.Printf("ooo shit %d", i)
		go func() {
			err := PortForwardAPod(PortForwardAPodRequest{
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
				println("ooo shit")
				panic(err)
			}
		}()
		select {
		case <-readyCh:
			i++
			continue
		}

	}

	out0, _ := exec.Command("curl", "https://127.0.0.1:8080", "-k").Output()
	// require.NoError(t, err)
	t.Logf("out: %v", out0)
	out1, _ := exec.Command("curl", "https://127.0.0.1:8081", "-k").Output()
	// require.NoError(t, err)
	t.Logf("out: %v", out1)
	out2, _ := exec.Command("curl", "https://127.0.0.1:8082", "-k").Output()
	// require.NoError(t, err)
	t.Logf("out: %v", out2)

	println("Port forwarding is ready to get traffic. have fun!")
	wg.Done()

}

func TestOperatorPod(t *testing.T) {
	kubeConfig, err := test.NewClientConfigForTest()
	require.NoError(t, err)
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	require.NoError(t, err)
	podList, err := kubeClient.CoreV1().Pods("openshift-etcd").List(context.TODO(), metav1.ListOptions{LabelSelector: "app=etcd"})
	require.NoError(t, err)
	out, out2, _ := ExecuteRemoteCommand(&podList.Items[0], "/bin/sh", "-c", "cat $ETCDCTL_CACERT | base64")
	//	require.NoError(t, err)
	t.Logf("out: %v, %v", out, out2)

	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		pair := strings.SplitN(scanner.Text(), "=", 2)
		if pair[0] != "" && pair[0] == "ETCDCTL_CACERT" {
			t.Logf("found %s\n", scanner.Text())
		}
	}
}

func PortForwardAPod(req PortForwardAPodRequest) error {
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

func ExecuteRemoteCommand(pod *v1.Pod, cmd ...string) (string, string, error) {
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
