package ceohelpers

import (
	"context"

	"github.com/openshift/library-go/pkg/operator/v1helpers"
	"k8s.io/klog/v2"
)

// TODO maybe get this into openshift/library-go
type StaticPodOperatorClientWithFinalizers interface {
	v1helpers.StaticPodOperatorClient
	v1helpers.OperatorClientWithFinalizers
}

type staticPodOperatorClientWithFinalizers struct {
	v1helpers.StaticPodOperatorClient
}

func NewStaticPodOperatorClientWithFinalizers(
	operatorClient v1helpers.StaticPodOperatorClient,
) StaticPodOperatorClientWithFinalizers {
	return staticPodOperatorClientWithFinalizers{
		operatorClient,
	}
}

func (c staticPodOperatorClientWithFinalizers) EnsureFinalizer(ctx context.Context, finalizer string) error {
	// TODO do we need this?
	klog.Infof("[NOT IMPLEMENTED] Should add finalizer: %s", finalizer)
	return nil
}

func (s staticPodOperatorClientWithFinalizers) RemoveFinalizer(ctx context.Context, finalizer string) error {
	// TODO do we need this?
	klog.Infof("[NOT IMPLEMENTED] Should remove finalizer: %s", finalizer)
	return nil
}
