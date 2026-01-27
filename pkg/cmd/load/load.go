package load

import (
	"context"
	goflag "flag"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdclientv1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

type loadOpts struct {
	loadGigabytes  int
	kubeconfigPath string
}

func newLoadOpts() *loadOpts {
	return &loadOpts{}
}

func NewLoadCommand(ctx context.Context) *cobra.Command {
	opts := newLoadOpts()
	cmd := &cobra.Command{
		Use:   "load",
		Short: "Loads several gigabytes of CRDs into the cluster via the apiserver.",
		Run: func(cmd *cobra.Command, args []string) {
			defer klog.Flush()

			if err := opts.Run(ctx); err != nil {
				klog.Fatal(err)
			}
		},
	}

	opts.AddFlags(cmd)
	return cmd
}

func (r *loadOpts) AddFlags(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.IntVar(&r.loadGigabytes, "g", 8, "How many gigabytes to load, defaults to 8")
	fs.StringVar(&r.kubeconfigPath, "kubeconfig", "", "Path to the kubeconfig file to use for CLI requests, uses in-cluster config otherwise")

	// adding klog flags to tune verbosity better
	gfs := goflag.NewFlagSet("", goflag.ExitOnError)
	klog.InitFlags(gfs)
	cmd.Flags().AddGoFlagSet(gfs)
}

func (r *loadOpts) Run(ctx context.Context) error {
	var clientConfig *rest.Config
	if r.kubeconfigPath != "" {
		cc, err := clientcmd.BuildConfigFromFlags("", r.kubeconfigPath)
		if err != nil {
			return err
		}
		clientConfig = cc
	} else {
		klog.Infof("assuming in-cluster config")
		cc, err := rest.InClusterConfig()
		if err != nil {
			return err
		}
		clientConfig = cc
	}

	protoConfig := rest.CopyConfig(clientConfig)
	protoConfig.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	protoConfig.ContentType = "application/vnd.kubernetes.protobuf"
	kubeClient, err := kubernetes.NewForConfig(protoConfig)
	if err != nil {
		return err
	}
	dynClient, err := dynamic.NewForConfig(protoConfig)
	if err != nil {
		return err
	}
	crdClient, err := crdclientv1.NewForConfig(protoConfig)
	if err != nil {
		return err
	}

	randSuffix := randSeq(5)
	klog.Info("generated random suffix: " + randSuffix)
	randomContent := randSeq(1024 * 1024 * 1.25)

	for i := 0; i < r.loadGigabytes; i++ {
		klog.Infof("loading gigabyte #%d", i+1)
		ns, err := r.createNamespace(ctx, kubeClient, randSuffix, i)
		if err != nil {
			return err
		}
		klog.Infof("created namespace %s", ns.Name)

		createdCrd, err := r.createCustomResource(ctx, crdClient, randSuffix, i)
		if err != nil {
			return err
		}

		klog.Infof("created CRD %s", createdCrd.Name)

		gvr := schema.GroupVersionResource{Group: createdCrd.Spec.Group, Version: "v1", Resource: createdCrd.Spec.Names.Kind}
		klog.Infof("waiting for CRD %s to become ready...", createdCrd.Name)
		err = r.waitForCRD(ctx, createdCrd, randSuffix, ns, dynClient, gvr)
		if err != nil {
			return err
		}

		klog.Infof("CRD %s is ready, loading a gigabyte of data now...", createdCrd.Name)
		// one CRD will create about 1.3mb worth of data, we want to create about a gigabyte worth of it
		numIterations := 800
		for j := 0; j < numIterations; j++ {
			err := retry.OnError(wait.Backoff{Steps: 10, Duration: 1 * time.Second, Factor: 2, Jitter: 0.1}, func(err error) bool {
				sErr := err.Error()
				return errors.IsServerTimeout(err) || errors.IsConflict(err) ||
					errors.IsTooManyRequests(err) || errors.IsServiceUnavailable(err) ||
					errors.IsTimeout(err) || strings.Contains(sErr, "etcdserver: request timed out") ||
					strings.Contains(sErr, "unexpected EOF") ||
					strings.Contains(sErr, "context deadline exceeded") ||
					strings.Contains(sErr, "rpc error: code = Unavailable") ||
					strings.Contains(sErr, "connection reset by peer")
			}, func() error {
				k := &unstructured.Unstructured{Object: map[string]interface{}{
					"apiVersion": gvr.GroupVersion().String(),
					"kind":       createdCrd.Spec.Names.Kind,
					"metadata": map[string]interface{}{
						"name":      fmt.Sprintf("ceo-load-inst-%s-%d", randSuffix, j),
						"namespace": ns.Name,
					},
					"spec": map[string]interface{}{
						"content": randomContent,
					},
				}}

				_, err := dynClient.Resource(gvr).Namespace(ns.Name).Create(ctx, k, metav1.CreateOptions{})
				if err != nil {
					klog.Errorf("failed to create instance of CRD %s/%s in namespace %s: %v", createdCrd.Name, k.GetName(), ns.Name, err)
					return err
				}

				return nil
			})
			if err != nil {
				return err
			}

			if j%10 == 0 {
				klog.Infof("created CRD instance %d/%d of CRD %s in namespace %s", j, numIterations, createdCrd.Name, ns.Name)
			}
		}
	}

	klog.Info("done")

	return nil
}

func (r *loadOpts) waitForCRD(ctx context.Context, createdCrd *v1.CustomResourceDefinition, randSuffix string, ns *corev1.Namespace, dynClient *dynamic.DynamicClient, gvr schema.GroupVersionResource) error {
	// the discovery of the CRD may take some time and thus the api would return 404 errors in the meantime
	return retry.OnError(wait.Backoff{Steps: 10, Duration: 5 * time.Second, Factor: 2, Jitter: 0.1}, errors.IsNotFound, func() error {
		k := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": gvr.GroupVersion().String(),
			"kind":       createdCrd.Spec.Names.Kind,
			"metadata": map[string]interface{}{
				"name":      fmt.Sprintf("ceo-load-inst-%s-test", randSuffix),
				"namespace": ns.Name,
			},
			"spec": map[string]interface{}{
				"content": "test",
			},
		}}
		_, err := dynClient.Resource(gvr).Namespace(ns.Name).Create(ctx, k, metav1.CreateOptions{})
		if err != nil {
			klog.Errorf("failed to create test instance of CRD %s/%s in namespace %s: %v", createdCrd.Name, k.GetName(), ns.Name, err)
			return err
		}
		return nil
	})
}

func (r *loadOpts) createNamespace(ctx context.Context, kubeClient *kubernetes.Clientset, randSuffix string, i int) (*corev1.Namespace, error) {
	return kubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("ceo-load-%s-%d", randSuffix, i)}}, metav1.CreateOptions{})
}

func (r *loadOpts) createCustomResource(ctx context.Context, crdClient *crdclientv1.Clientset, randSuffix string, i int) (*v1.CustomResourceDefinition, error) {
	crd := &v1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("load-crd-%s-%d.g%d.openshift.com", randSuffix, i, i),
		},
		Spec: v1.CustomResourceDefinitionSpec{
			Group: fmt.Sprintf("g%d.openshift.com", i),
			Versions: []v1.CustomResourceDefinitionVersion{
				{Name: "v1",
					Served:  true,
					Storage: true,
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{Type: "object",
							Properties: map[string]v1.JSONSchemaProps{
								"spec": {Type: "object", Properties: map[string]v1.JSONSchemaProps{
									"content": {Type: "string"},
								}},
							}},
					}},
			},
			Scope: v1.NamespaceScoped,
			Names: v1.CustomResourceDefinitionNames{
				Plural:   fmt.Sprintf("load-crd-%s-%d", randSuffix, i),
				Singular: fmt.Sprintf("load-crd-%s-%d", randSuffix, i),
				Kind:     fmt.Sprintf("load-crd-%s-%d", randSuffix, i),
				ShortNames: []string{
					fmt.Sprintf("lcrd-%s-%d", randSuffix, i),
				},
			},
		},
	}
	createdCrd, err := crdClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	return createdCrd, nil
}

func randSeq(n int) string {
	l := []rune("abcdefghijklmnopqrstuvwxyz")
	b := make([]rune, n)
	for i := range b {
		b[i] = l[rand.Intn(len(l))]
	}
	return string(b)
}
