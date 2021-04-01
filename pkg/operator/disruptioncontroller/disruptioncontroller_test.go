package disruptioncontroller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/openshift/library-go/pkg/operator/events"
	"go.etcd.io/etcd/integration"
	"k8s.io/apimachinery/pkg/api/equality"
)

func Test_checkClientConnConnectivityRestored(t *testing.T) {
	expected := []string{"ConnectivityOutageDetected", "ConnectivityRestored"}
	clus := integration.NewClusterV3(t, &integration.ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	eventRecorder := events.NewInMemoryRecorder("")
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := clus.Client(0).Dial(clus.Client(0).Endpoints()[0])
	if err != nil {
		t.Fatal(err)
	}
	// stop member 0
	clus.Members[0].Stop(t)

	// restart
	tf := time.AfterFunc(time.Second*3, func() {
		clus.Members[0].Restart(t)
	})
	defer tf.Stop()

	wg.Add(1)
	go checkClientConn(ctx, conn, clus.Members[0].Name, eventRecorder, &wg)
	wg.Wait()

	var events []string
	for _, ev := range eventRecorder.Events() {
		events = append(events, ev.Reason)
	}
	if got := events; !equality.Semantic.DeepEqual(got, expected) {
		t.Errorf("unexpected events, got reasons: %v, expected: %v", got, expected)
	}
}

func Test_checkClientConnReadyTimeout(t *testing.T) {
	var expected []string
	clus := integration.NewClusterV3(t, &integration.ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	eventRecorder := events.NewInMemoryRecorder("")
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := clus.Client(0).Dial(clus.Client(0).Endpoints()[0])
	if err != nil {
		t.Fatal(err)
	}

	wg.Add(1)
	go checkClientConn(ctx, conn, clus.Members[0].Name, eventRecorder, &wg)
	wg.Wait()

	var events []string
	for _, ev := range eventRecorder.Events() {
		events = append(events, ev.Reason)
	}
	if got := events; !equality.Semantic.DeepEqual(got, expected) {
		t.Errorf("unexpected events, got reasons: %v, expected: %v", got, expected)
	}
}

func Test_checkClientConnMultipleOutage(t *testing.T) {
	expected := []string{"ConnectivityOutageDetected", "ConnectivityRestored", "ConnectivityOutageDetected", "ConnectivityRestored"}
	clus := integration.NewClusterV3(t, &integration.ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	eventRecorder := events.NewInMemoryRecorder("")
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := clus.Client(0).Dial(clus.Client(0).Endpoints()[0])
	if err != nil {
		t.Fatal(err)
	}
	// stop member 0
	clus.Members[0].Stop(t)

	// restart
	tf1 := time.AfterFunc(time.Second*3, func() {
		clus.Members[0].Restart(t)
	})
	defer tf1.Stop()

	// stop
	tf2 := time.AfterFunc(time.Second*9, func() {
		clus.Members[0].Stop(t)
	})
	defer tf2.Stop()

	// restart
	tf3 := time.AfterFunc(time.Second*15, func() {
		clus.Members[0].Restart(t)
	})
	defer tf3.Stop()

	wg.Add(1)
	go checkClientConn(ctx, conn, clus.Members[0].Name, eventRecorder, &wg)
	wg.Wait()

	var events []string
	for _, ev := range eventRecorder.Events() {
		events = append(events, ev.Reason)
	}
	if got := events; !equality.Semantic.DeepEqual(got, expected) {
		t.Errorf("unexpected events, got reasons: %v, expected: %v", got, expected)
	}
}

func Test_checkClientConnDisruptionTimeout(t *testing.T) {
	expected := []string{"ConnectivityOutageDetected", "ConnectivityOutagePaused"}
	clus := integration.NewClusterV3(t, &integration.ClusterConfig{Size: 3})
	defer clus.Terminate(t)

	eventRecorder := events.NewInMemoryRecorder("")
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := clus.Client(0).Dial(clus.Client(0).Endpoints()[0])
	if err != nil {
		t.Fatal(err)
	}

	// stop member 0
	clus.Members[0].Stop(t)

	wg.Add(1)
	go checkClientConn(ctx, conn, clus.Members[0].Name, eventRecorder, &wg)
	wg.Wait()

	var events []string
	for _, ev := range eventRecorder.Events() {
		events = append(events, ev.Reason)
	}
	if got := events; !equality.Semantic.DeepEqual(got, expected) {
		t.Errorf("unexpected events, got reasons: %v, expected: %v", got, expected)
	}
}
