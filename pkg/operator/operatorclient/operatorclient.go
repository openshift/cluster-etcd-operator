package operatorclient

import (
	"k8s.io/client-go/tools/cache"

	operatorv1 "github.com/openshift/api/operator/v1"
	operatorv1client "github.com/openshift/client-go/operator/clientset/versioned/typed/operator/v1"
	operatorv1informers "github.com/openshift/client-go/operator/informers/externalversions"
)

type OperatorClient struct {
	Informers operatorv1informers.SharedInformerFactory
	Client    operatorv1client.EtcdsGetter
}

func (c *OperatorClient) Informer() cache.SharedIndexInformer {
	return c.Informers.Operator().V1().Etcds().Informer()
}

func (c *OperatorClient) GetStaticPodOperatorState() (*operatorv1.StaticPodOperatorSpec, *operatorv1.StaticPodOperatorStatus, string, error) {
	instance, err := c.Informers.Operator().V1().Etcds().Lister().Get("cluster")
	if err != nil {
		return nil, nil, "", err
	}

	return &instance.Spec.StaticPodOperatorSpec, &instance.Status.StaticPodOperatorStatus, instance.ResourceVersion, nil
}

func (c *OperatorClient) UpdateStaticPodOperatorStatus(resourceVersion string, status *operatorv1.StaticPodOperatorStatus) (*operatorv1.StaticPodOperatorStatus, error) {
	original, err := c.Informers.Operator().V1().Etcds().Lister().Get("cluster")
	if err != nil {
		return nil, err
	}
	copy := original.DeepCopy()
	copy.ResourceVersion = resourceVersion
	copy.Status.StaticPodOperatorStatus = *status

	ret, err := c.Client.Etcds().UpdateStatus(copy)
	if err != nil {
		return nil, err
	}

	return &ret.Status.StaticPodOperatorStatus, nil
}

func (c *OperatorClient) GetOperatorState() (*operatorv1.OperatorSpec, *operatorv1.OperatorStatus, string, error) {
	instance, err := c.Informers.Operator().V1().Etcds().Lister().Get("cluster")
	if err != nil {
		return nil, nil, "", err
	}

	return &instance.Spec.OperatorSpec, &instance.Status.StaticPodOperatorStatus.OperatorStatus, instance.ResourceVersion, nil
}

func (c *OperatorClient) UpdateOperatorSpec(resourceVersion string, spec *operatorv1.OperatorSpec) (*operatorv1.OperatorSpec, string, error) {
	original, err := c.Informers.Operator().V1().Etcds().Lister().Get("cluster")
	if err != nil {
		return nil, "", err
	}
	copy := original.DeepCopy()
	copy.ResourceVersion = resourceVersion
	copy.Spec.OperatorSpec = *spec

	ret, err := c.Client.Etcds().Update(copy)
	if err != nil {
		return nil, "", err
	}

	return &ret.Spec.OperatorSpec, ret.ResourceVersion, nil
}
func (c *OperatorClient) UpdateOperatorStatus(resourceVersion string, status *operatorv1.OperatorStatus) (*operatorv1.OperatorStatus, error) {
	original, err := c.Informers.Operator().V1().Etcds().Lister().Get("cluster")
	if err != nil {
		return nil, err
	}
	copy := original.DeepCopy()
	copy.ResourceVersion = resourceVersion
	copy.Status.StaticPodOperatorStatus.OperatorStatus = *status

	ret, err := c.Client.Etcds().UpdateStatus(copy)
	if err != nil {
		return nil, err
	}

	return &ret.Status.StaticPodOperatorStatus.OperatorStatus, nil
}
