package certrotation

import (
	"context"
	"fmt"
	"time"

	operatorv1 "github.com/openshift/api/operator/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/condition"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

const (
	// CertificateNotBeforeAnnotation contains the certificate expiration date in RFC3339 format.
	CertificateNotBeforeAnnotation = "auth.openshift.io/certificate-not-before"
	// CertificateNotAfterAnnotation contains the certificate expiration date in RFC3339 format.
	CertificateNotAfterAnnotation = "auth.openshift.io/certificate-not-after"
	// CertificateIssuer contains the common name of the certificate that signed another certificate.
	CertificateIssuer = "auth.openshift.io/certificate-issuer"
	// CertificateHostnames contains the hostnames used by a signer.
	CertificateHostnames = "auth.openshift.io/certificate-hostnames"
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
	Name string
	// RotatedSigningCASecret rotates a self-signed signing CA stored in a secret.
	RotatedSigningCASecret RotatedSigningCASecret
	// CABundleConfigMap maintains a CA bundle config map, by adding new CA certs coming from rotatedSigningCASecret, and by removing expired old ones.
	CABundleConfigMap CABundleConfigMap
	// RotatedTargetSecrets contains a list of key and cert signed by a signing CA to rotate.
	RotatedTargetSecrets []RotatedSelfSignedCertKeySecret
	// Plumbing:
	StatusReporter StatusReporter
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
		Name:                   name,
		RotatedSigningCASecret: rotatedSigningCASecret,
		CABundleConfigMap:      caBundleConfigMap,
		RotatedTargetSecrets:   []RotatedSelfSignedCertKeySecret{rotatedSelfSignedCertKeySecret},
		StatusReporter:         reporter,
	}
	return factory.New().
		ResyncEvery(time.Minute).
		WithSync(c.Sync).
		WithInformers(
			rotatedSigningCASecret.Informer.Informer(),
			caBundleConfigMap.Informer.Informer(),
			rotatedSelfSignedCertKeySecret.Informer.Informer(),
		).
		WithPostStartHooks(
			c.targetCertRecheckerPostRunHook,
		).
		ToController("CertRotationController", recorder.WithComponentSuffix("cert-rotation-controller").WithComponentSuffix(name))
}

func NewCertRotationControllerMultipleTargets(
	name string,
	rotatedSigningCASecret RotatedSigningCASecret,
	caBundleConfigMap CABundleConfigMap,
	rotatedTargetSecrets []RotatedSelfSignedCertKeySecret,
	recorder events.Recorder,
	reporter StatusReporter,
) factory.Controller {
	informers := sets.New[factory.Informer](
		rotatedSigningCASecret.Informer.Informer(),
		caBundleConfigMap.Informer.Informer(),
	)

	for _, target := range rotatedTargetSecrets {
		informers = informers.Insert(target.Informer.Informer())
	}

	c := &CertRotationController{
		Name:                   name,
		RotatedSigningCASecret: rotatedSigningCASecret,
		CABundleConfigMap:      caBundleConfigMap,
		RotatedTargetSecrets:   rotatedTargetSecrets,
		StatusReporter:         reporter,
	}
	return factory.New().
		ResyncEvery(time.Minute).
		WithSync(c.Sync).
		WithInformers(
			informers.UnsortedList()...,
		).
		WithPostStartHooks(
			c.targetCertRecheckerPostRunHook,
		).
		ToController("CertRotationController", recorder.WithComponentSuffix("cert-rotation-controller").WithComponentSuffix(name))
}

func (c CertRotationController) Sync(ctx context.Context, syncCtx factory.SyncContext) error {
	syncErr := c.SyncWorker(ctx)

	// running this function with RunOnceContextKey value context will make this "run-once" without updating status.
	isRunOnce, ok := ctx.Value(RunOnceContextKey).(bool)
	if ok && isRunOnce {
		return syncErr
	}

	updated, updateErr := c.StatusReporter.Report(ctx, c.Name, syncErr)
	if updateErr != nil {
		return updateErr
	}
	if updated && syncErr != nil {
		syncCtx.Recorder().Warningf("RotationError", syncErr.Error())
	}

	return syncErr
}

func (c CertRotationController) SyncWorker(ctx context.Context) error {
	signingCertKeyPair, _, err := c.RotatedSigningCASecret.EnsureSigningCertKeyPair(ctx)
	if err != nil {
		return err
	}

	cabundleCerts, err := c.CABundleConfigMap.EnsureConfigMapCABundle(ctx, signingCertKeyPair)
	if err != nil {
		return err
	}

	var errs []error
	for _, secret := range c.RotatedTargetSecrets {
		if _, err := secret.EnsureTargetCertKeyPair(ctx, signingCertKeyPair, cabundleCerts); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) != 0 {
		return errors.NewAggregate(errs)
	}

	return nil
}

func (c CertRotationController) targetCertRecheckerPostRunHook(ctx context.Context, syncCtx factory.SyncContext) error {
	// If we have a need to force rechecking the cert, use this channel to do it.
	var targetRefreshers []<-chan struct{}
	for _, target := range c.RotatedTargetSecrets {
		if refresher, ok := target.CertCreator.(TargetCertRechecker); ok {
			targetRefreshers = append(targetRefreshers, refresher.RecheckChannel())
		}
	}
	aggregateTargetRefresher := make(chan struct{})
	for _, ch := range targetRefreshers {
		go func(c <-chan struct{}) {
			for msg := range c {
				aggregateTargetRefresher <- msg
			}
		}(ch)
	}

	go wait.Until(func() {
		for {
			select {
			case <-aggregateTargetRefresher:
				syncCtx.Queue().Add(factory.DefaultQueueKey)
			case <-ctx.Done():
				return
			}
		}
	}, time.Minute, ctx.Done())

	<-ctx.Done()
	return nil
}
