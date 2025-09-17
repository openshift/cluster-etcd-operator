package setup

import (
	"context"
	"fmt"
	"os"
	"time"

	configclient "github.com/openshift/client-go/config/clientset/versioned"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions"
	operatorversionedclient "github.com/openshift/client-go/operator/clientset/versioned"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilclock "k8s.io/utils/clock"

	"github.com/openshift/cluster-etcd-operator/pkg/etcdcli"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/config"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/etcd"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/pcs"
	"github.com/openshift/cluster-etcd-operator/pkg/tnf/pkg/tools"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

func RunTnfSetup() error {
	klog.Info("Setting up clients etc. for TNF setup")

	ctx, cancel := context.WithCancel(context.Background())
	shutdownHandler := server.SetupSignalHandler()
	go func() {
		defer cancel()
		<-shutdownHandler
		klog.Info("Received SIGTERM or SIGINT signal, terminating")
	}()

	clientConfig, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	protoConfig := rest.CopyConfig(clientConfig)
	protoConfig.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	protoConfig.ContentType = "application/vnd.kubernetes.protobuf"

	// This kube client use protobuf, do not use it for CR
	kubeClient, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	// CRD clients
	configClient, err := configclient.NewForConfig(clientConfig)
	if err != nil {
		return err
	}
	kubeInformers := v1helpers.NewKubeInformersForNamespaces(
		kubeClient,
		operatorclient.TargetNamespace,
		"",
	)
	nodes := kubeInformers.InformersFor("").Core().V1().Nodes()

	cfgInformers := configv1informers.NewSharedInformerFactory(configClient, 0)
	networkInformer := cfgInformers.Config().V1().Networks()

	recorder := events.NewInMemoryRecorder("tnf-fencing", utilclock.RealClock{})

	stopCh := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(stopCh)
	}()
	kubeInformers.Start(stopCh)
	cfgInformers.Start(stopCh)

	if ok := cache.WaitForCacheSync(
		ctx.Done(),
		nodes.Informer().HasSynced,
		networkInformer.Informer().HasSynced,
	); !ok {
		return fmt.Errorf("failed to sync informers for etcd client")
	}

	ec := etcdcli.NewEtcdClient(
		kubeInformers,
		nodes.Informer(),
		nodes.Lister(),
		networkInformer,
		recorder,
	)

	operatorConfigClient, err := operatorversionedclient.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	klog.Info("Waiting for completed auth jobs")
	authDone := func(context.Context) (done bool, err error) {
		jobs, err := kubeClient.BatchV1().Jobs("openshift-etcd").List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app.kubernetes.io/name=%s", tools.JobTypeAuth.GetNameLabelValue()),
		})
		if err != nil {
			klog.Warningf("Failed to list jobs: %v", err)
			return false, nil
		}
		if jobs.Items == nil || len(jobs.Items) != 2 {
			klog.Warningf("Expected 2 jobs, got %d", len(jobs.Items))
		}
		for _, job := range jobs.Items {
			if !tools.IsConditionTrue(job.Status.Conditions, batchv1.JobComplete) {
				klog.Warningf("Job %s not complete", job.Name)
				return false, nil
			}
		}
		klog.Info("Auth jobs completed successfully")
		return true, nil
	}
	err = wait.PollUntilContextTimeout(ctx, tools.JobPollIntervall, tools.AuthJobCompletedTimeout, true, authDone)
	if err != nil {
		klog.Errorf("Timed out waiting for auth jobs to complete: %v", err)
		return err
	}

	klog.Info("Running TNF setup")

	// create tnf cluster config
	cfg, err := config.GetClusterConfig(ctx, kubeClient)
	if err != nil {
		return err
	}

	// configure pcs cluster
	configured, err := pcs.ConfigureCluster(ctx, cfg)
	if err != nil {
		return err
	} else if configured {
		// give the cluster some time for sync
		time.Sleep(5 * time.Second)
	}

	// configure stonith
	err = pcs.ConfigureFencing(ctx, kubeClient, cfg)
	if err != nil {
		return err
	}

	// Etcd handover

	// configure etcd resource - it won't start etcd before CEO managed etcd is removed per node
	err = pcs.ConfigureEtcd(ctx, cfg)
	if err != nil {
		return err
	}

	// configure etcd constraints
	configured, err = pcs.ConfigureConstraints(ctx)
	if err != nil {
		return err
	} else if configured {
		// give the cluster some time for sync
		time.Sleep(5 * time.Second)
	}

	// remove CEO managed etcd container
	err = etcd.RemoveStaticContainer(ctx, operatorConfigClient)
	if err != nil {
		return err
	}

	// get pcs cib
	cib, err := pcs.GetCIB(ctx)
	if err != nil {
		return err
	}

	if os.Getenv("TNF_VALIDATE_PEER_ONLY") == "true" {
		if err := pcs.ValidateFencingPeerOnly(ctx, cfg, ec); err != nil {
			return fmt.Errorf("peer-only disruptive validation failed: %w", err)
		}
	}
	klog.Infof("HA setup done! CIB:\n%s", cib)

	return nil
}
