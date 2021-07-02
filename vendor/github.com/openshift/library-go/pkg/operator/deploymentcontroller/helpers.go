package deploymentcontroller

import (
	"fmt"
	"os"

	opv1 "github.com/openshift/api/operator/v1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// WithReplicasHook sets the deployment.Spec.Replicas field according to the number
// of available nodes. The number of nodes is determined by the node
// selector specified in the field deployment.Spec.Templates.NodeSelector.
func WithReplicasHook(nodeLister corev1listers.NodeLister) DeploymentHookFunc {
	return func(_ *opv1.OperatorSpec, deployment *appsv1.Deployment) error {
		nodeSelector := deployment.Spec.Template.Spec.NodeSelector
		nodes, err := nodeLister.List(labels.SelectorFromSet(nodeSelector))
		if err != nil {
			return err
		}
		replicas := int32(len(nodes))
		deployment.Spec.Replicas = &replicas
		return nil
	}
}

// WithImageHook sets the image associated with the deployment with the value provided
// for CLI_IMAGE env variable.
func WithImageHook() DeploymentHookFunc {
	return func(_ *opv1.OperatorSpec, deployment *appsv1.Deployment) error {
		image := os.Getenv("CLI_IMAGE")
		if image == "" {
			return fmt.Errorf("CLI_IMAGE is not populated")
		}
		deployment.Spec.Template.Spec.Containers[0].Image = image
		return nil
	}
}
