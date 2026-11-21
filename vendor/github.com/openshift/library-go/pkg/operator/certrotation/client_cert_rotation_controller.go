package certrotation

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/condition"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/clock"
)

const (
	// RunOnceContextKey is a context value key that can be used to call the controller Sync() and make it only run the syncWorker once and report error.
	RunOnceContextKey = "cert-rotation-controller.openshift.io/run-once"
)

// StatusReporter knows how to report the status of cert rotation
type StatusReporter interface {
	Report(ctx context.Context, controllerName string, syncErr error) (updated bool, updateErr error)
}

var _ StatusReporter = (*StaticPodConditionStatusReporter)(nil)

type StaticPodConditionStatusReporter struct {
	// Plumbing:
	OperatorClient v1helpers.StaticPodOperatorClient
}

func (s *StaticPodConditionStatusReporter) Report(ctx context.Context, controllerName string, syncErr error) (bool, error) {
	newCondition := operatorv1.OperatorCondition{
		Type:   fmt.Sprintf(condition.CertRotationDegradedConditionTypeFmt, controllerName),
		Status: operatorv1.ConditionFalse,
	}
	if syncErr != nil {
		newCondition.Status = operatorv1.ConditionTrue
		newCondition.Reason = "RotationError"
		newCondition.Message = syncErr.Error()
	}
	_, updated, updateErr := v1helpers.UpdateStaticPodStatus(ctx, s.OperatorClient, v1helpers.UpdateStaticPodConditionFn(newCondition))
	return updated, updateErr
}

// CertRotationController does:
//
// 1) continuously create a self-signed signing CA (via RotatedSigningCASecret) and store it in a secret.
// 2) maintain a CA bundle ConfigMap with all not yet expired CA certs.
// 3) continuously create a target cert and key signed by the latest signing CA and store it in a secret.
type CertRotationController struct {
	// controller name
	name string
	// RotatedSigningCASecret rotates a self-signed signing CA stored in a secret.
	rotatedSigningCASecret RotatedSigningCASecret
	// CABundleConfigMap maintains a CA bundle config map, by adding new CA certs coming from rotatedSigningCASecret, and by removing expired old ones.
	caBundleConfigMap CABundleConfigMap
	// RotatedSelfSignedCertKeySecret rotates a key and cert signed by a signing CA and stores it in a secret.
	rotatedSelfSignedCertKeySecret RotatedSelfSignedCertKeySecret

	// Plumbing:
	statusReporter StatusReporter

	clock clock.PassiveClock
}

func NewCertRotationController(
	name string,
	rotatedSigningCASecret RotatedSigningCASecret,
	caBundleConfigMap CABundleConfigMap,
	rotatedSelfSignedCertKeySecret RotatedSelfSignedCertKeySecret,
	recorder events.Recorder,
	reporter StatusReporter,
) factory.Controller {
	c := &CertRotationController{
		name:                           name,
		rotatedSigningCASecret:         rotatedSigningCASecret,
		caBundleConfigMap:              caBundleConfigMap,
		rotatedSelfSignedCertKeySecret: rotatedSelfSignedCertKeySecret,
		statusReporter:                 reporter,
		clock:                          clock.RealClock{},
	}

	return factory.New().
		ResyncEvery(time.Minute).
		WithSync(c.Sync).
		WithFilteredEventsInformers(
			func(obj interface{}) bool {
				if cm, ok := obj.(*corev1.ConfigMap); ok {
					return cm.Namespace == caBundleConfigMap.namespace && cm.Name == caBundleConfigMap.name
				}
				if secret, ok := obj.(*corev1.Secret); ok {
					if secret.Namespace == rotatedSigningCASecret.namespace && secret.Name == rotatedSigningCASecret.name {
						return true
					}
					if secret.Namespace == rotatedSelfSignedCertKeySecret.namespace && secret.Name == rotatedSelfSignedCertKeySecret.name {
						return true
					}
					return false
				}
				return true
			},
			rotatedSigningCASecret.informer.Informer(),
			caBundleConfigMap.informer.Informer(),
			rotatedSelfSignedCertKeySecret.informer.Informer(),
		).
		WithPostStartHooks(
			c.targetCertRecheckerPostRunHook,
		).
		ToController(
			"CertRotationController", // don't change what is passed here unless you also remove the old FooDegraded condition
			recorder.WithComponentSuffix("cert-rotation-controller").WithComponentSuffix(name),
		)
}

func (c CertRotationController) Sync(ctx context.Context, syncCtx factory.SyncContext) error {
	syncErr := c.SyncWorker(ctx)

	// running this function with RunOnceContextKey value context will make this "run-once" without updating status.
	isRunOnce, ok := ctx.Value(RunOnceContextKey).(bool)
	if ok && isRunOnce {
		return syncErr
	}

	updated, updateErr := c.statusReporter.Report(ctx, c.name, syncErr)
	if updateErr != nil {
		return updateErr
	}
	if updated && syncErr != nil {
		syncCtx.Recorder().Warningf("RotationError", "%s", syncErr.Error())
	}

	return syncErr
}

func (c CertRotationController) getSigningCertKeyPairLocation() string {
	return fmt.Sprintf("%s/%s", c.rotatedSelfSignedCertKeySecret.namespace, c.rotatedSelfSignedCertKeySecret.name)
}

func (c CertRotationController) SyncWorker(ctx context.Context) error {
	signingCertKeyPair, _, err := c.rotatedSigningCASecret.EnsureSigningCertKeyPair(ctx)
	if err != nil {
		return fmt.Errorf("ensuring signing cert keypair: %w", err)
	}
	// If no signingCertKeyPair returned due to update conflict or otherwise, return an error
	if signingCertKeyPair == nil {
		return fmt.Errorf("signingCertKeyPair is nil")
	}

	cabundleCerts, err := c.caBundleConfigMap.EnsureConfigMapCABundle(ctx, signingCertKeyPair, c.getSigningCertKeyPairLocation())
	if err != nil {
		return fmt.Errorf("ensuring configmap CA bundle: %w", err)
	}
	// If no ca bundle returned due to update conflict or otherwise, return an error
	if cabundleCerts == nil {
		return fmt.Errorf("cabundleCerts is nil")
	}

	if _, err := c.rotatedSelfSignedCertKeySecret.EnsureTargetCertKeyPair(ctx, signingCertKeyPair, cabundleCerts); err != nil {
		return fmt.Errorf("ensuring target cert keypair: %w", err)
	}

	return nil
}

func (c CertRotationController) targetCertRecheckerPostRunHook(ctx context.Context, syncCtx factory.SyncContext) error {
	// If we have a need to force rechecking the cert, use this channel to do it.
	refresher, ok := c.rotatedSelfSignedCertKeySecret.certCreator.(TargetCertRechecker)
	if !ok {
		return nil
	}
	targetRefresh := refresher.RecheckChannel()
	go wait.Until(func() {
		for {
			select {
			case <-targetRefresh:
				syncCtx.Queue().Add(factory.DefaultQueueKey)
			case <-ctx.Done():
				return
			}
		}
	}, time.Minute, ctx.Done())

	<-ctx.Done()
	return nil
}
