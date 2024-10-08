package etcdcertcleaner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/health"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
	"github.com/openshift/cluster-etcd-operator/pkg/tlshelpers"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

const DeletionGracePeriodAnnotation = "openshift.io/ceo-delete-grace-start"
const DeletionGracePeriod = 24 * 3 * time.Hour

var dynamicCertPrefixes = []string{
	tlshelpers.GetPeerClientSecretNameForNode(""),
	tlshelpers.GetServingSecretNameForNode(""),
	tlshelpers.GetServingMetricsSecretNameForNode(""),
}

// EtcdCertCleanerController will clean up old and unused dynamically generated ("node-based") secrets.
// This is to avoid filling up etcd in environments where the control plane is rotated/scaled up and down very often.
type EtcdCertCleanerController struct {
	eventRecorder      events.Recorder
	kubeClient         kubernetes.Interface
	operatorClient     v1helpers.StaticPodOperatorClient
	masterNodeLister   corev1listers.NodeLister
	masterNodeSelector labels.Selector
	secretInformer     corev1informers.SecretInformer
	secretLister       corev1listers.SecretLister
	secretClient       corev1client.SecretsGetter
	quorumChecker      ceohelpers.QuorumChecker
}

func NewEtcdCertCleanerController(
	livenessChecker *health.MultiAlivenessChecker,
	kubeClient kubernetes.Interface,
	operatorClient v1helpers.StaticPodOperatorClient,
	kubeInformers v1helpers.KubeInformersForNamespaces,
	masterNodeLister corev1listers.NodeLister,
	masterNodeSelector labels.Selector,
	eventRecorder events.Recorder,
) factory.Controller {
	secretInformer := kubeInformers.InformersFor(operatorclient.TargetNamespace).Core().V1().Secrets()
	secretLister := secretInformer.Lister()
	secretClient := v1helpers.CachedSecretGetter(kubeClient.CoreV1(), kubeInformers)

	c := &EtcdCertCleanerController{
		eventRecorder:      eventRecorder,
		kubeClient:         kubeClient,
		operatorClient:     operatorClient,
		masterNodeLister:   masterNodeLister,
		masterNodeSelector: masterNodeSelector,
		secretInformer:     secretInformer,
		secretLister:       secretLister,
		secretClient:       secretClient,
	}

	// we estimate the rate of control plane scales to maybe 1-2 a day, so running this once an hour with 6h of health threshold
	// should be more than sufficient. Note that this controller does not sync on any informers.
	syncer := health.NewCheckingSyncWrapper(c.sync, 6*time.Hour)
	livenessChecker.Add("EtcdCertCleanerController", syncer)
	return factory.New().ResyncEvery(1*time.Hour).WithSync(syncer.Sync).ToController("EtcdCertCleanerController", c.eventRecorder)
}

// sync will try to find unused cert secrets which are then marked for deletion, after three days the secrets are deleted.
// This control loop does not implement "undelete". In case the secrets become managed by CertSignerController again, the
// deletion label is getting removed automatically - in the worst case the cert is getting recreated entirely.
func (c *EtcdCertCleanerController) sync(ctx context.Context, _ factory.SyncContext) error {
	secrets, err := c.findUnusedNodeBasedSecrets(ctx)
	if err != nil {
		return err
	}

	for _, secretToDelete := range secrets {
		klog.Infof("deleting secret [%s] detected unused for %s", secretToDelete.Name, DeletionGracePeriod)
		err := c.secretClient.Secrets(operatorclient.TargetNamespace).Delete(ctx, secretToDelete.Name, v1.DeleteOptions{})
		if err != nil {
			return fmt.Errorf("EtcdCertCleanerController: could not delete unused secret [%s], error was: %w", secretToDelete.Name, err)
		}
	}

	return nil
}

func (c *EtcdCertCleanerController) findUnusedNodeBasedSecrets(ctx context.Context) ([]*corev1.Secret, error) {
	secrets, err := c.secretLister.List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("EtcdCertCleanerController: could not list secrets, error was: %w", err)
	}

	secrets = filterNodeBasedSecrets(secrets)
	nodes, err := c.masterNodeLister.List(c.masterNodeSelector)
	if err != nil {
		return nil, fmt.Errorf("EtcdCertCleanerController: could not list control plane nodes, error was: %w", err)
	}

	var existingNodeSecrets []*corev1.Secret
	for _, n := range nodes {
		for i := 0; i < len(secrets); i++ {
			if strings.HasSuffix(secrets[i].Name, n.Name) {
				existingNodeSecrets = append(existingNodeSecrets, secrets[i])
			}
		}
	}

	for _, secretToUntag := range existingNodeSecrets {
		if secretToUntag.Annotations == nil {
			secretToUntag.Annotations = map[string]string{}
		}
		delete(secretToUntag.Annotations, DeletionGracePeriodAnnotation)
		klog.Infof("unused secret [%s] became used again, removing label for grace period", secretToUntag.Name)
		_, err := c.secretClient.Secrets(operatorclient.TargetNamespace).Update(ctx, secretToUntag, v1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("EtcdCertCleanerController: could not untag used secret [%s], error was: %w", secretToUntag.Name, err)
		}
	}

	var goneNodeSecrets []*corev1.Secret
	for i := 0; i < len(secrets); i++ {
		nodeFound := false
		for _, n := range nodes {
			if strings.HasSuffix(secrets[i].Name, n.Name) {
				nodeFound = true
				break
			}
		}

		if !nodeFound {
			goneNodeSecrets = append(goneNodeSecrets, secrets[i])
		}
	}

	var secretsRipeForDeletion []*corev1.Secret
	for _, secretToTag := range goneNodeSecrets {
		if secretToTag.Annotations == nil {
			secretToTag.Annotations = map[string]string{}
		}

		if _, ok := secretToTag.Annotations[DeletionGracePeriodAnnotation]; !ok {
			klog.Infof("unused secret [%s] detected, label for grace period of %s", secretToTag.Name, DeletionGracePeriod)
			secretToTag.Annotations[DeletionGracePeriodAnnotation] = time.Now().Format(time.RFC3339)
			_, err := c.secretClient.Secrets(operatorclient.TargetNamespace).Update(ctx, secretToTag, v1.UpdateOptions{})
			if err != nil {
				return nil, fmt.Errorf("EtcdCertCleanerController: could not tag unused secret [%s], error was: %w", secretToTag.Name, err)
			}
		} else {
			parsed, err := time.Parse(time.RFC3339, secretToTag.Annotations[DeletionGracePeriodAnnotation])
			if err != nil {
				return nil, fmt.Errorf("EtcdCertCleanerController: could not parse secret deletion grace period [%s], error was: %w", secretToTag.Name, err)
			}

			if time.Now().After(parsed.Add(DeletionGracePeriod)) {
				secretsRipeForDeletion = append(secretsRipeForDeletion, secretToTag.DeepCopy())
			}
		}
	}

	return secretsRipeForDeletion, nil
}

func filterNodeBasedSecrets(secrets []*corev1.Secret) []*corev1.Secret {
	var filtered []*corev1.Secret
	for _, s := range secrets {
		for _, prefix := range dynamicCertPrefixes {
			if strings.HasPrefix(s.Name, prefix) {
				filtered = append(filtered, s.DeepCopy())
				break
			}
		}
	}

	return filtered
}
