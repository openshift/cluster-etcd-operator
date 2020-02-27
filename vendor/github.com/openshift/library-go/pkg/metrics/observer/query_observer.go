package observer

import (
	"context"
	"fmt"
	"time"

	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	errorutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog"

	routev1client "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"

	operatorv1 "github.com/openshift/api/operator/v1"

	"github.com/openshift/library-go/pkg/controller/factory"
	metricsclient "github.com/openshift/library-go/pkg/metrics/client"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

var defaultPollingDuration = 30 * time.Second

type HandlerFunc = func(ctx context.Context, recorder events.Recorder, client prometheusv1.API) error

// Handler represents a single Prometheus query and a handler function that should run on result of given query.
type Handler struct {
	// name represents human readable name for this handler (this will be used when reporting errors) eg. "FsyncDurationSeconds"
	Name string
	// handler is used to send query to prometheus and interpret the results.
	Handler HandlerFunc
}

type prometheusMetricObserver struct {
	operatorClient v1helpers.StaticPodOperatorClient
	queryHandlers  []Handler

	serviceClient corev1client.ServicesGetter
	secretsClient corev1client.SecretsGetter
	configsClient corev1client.ConfigMapsGetter
	routeClient   routev1client.RoutesGetter

	// allow to mock the client in unit tests
	prometheusClientFn func(corev1client.ServicesGetter, corev1client.SecretsGetter, corev1client.ConfigMapsGetter, routev1client.RoutesGetter) (prometheusv1.API, error)
}

// NewPrometheusMetricObserver allows to continuously observe particular Prometheus query result value and react to it.
// The name should be unique name for this observer.
// The queryHandlers represents list of Prometheus queries and handlers where the result value and type are passed.
// The clients are needed to initialize that prometheus API client.
func NewPrometheusMetricObserver(name string, queryHandlers []Handler, serviceClient corev1client.ServicesGetter, secretsClient corev1client.SecretsGetter, configsClient corev1client.ConfigMapsGetter,
	routeClient routev1client.RoutesGetter, recorder events.Recorder, operatorClient v1helpers.StaticPodOperatorClient) factory.Controller {
	observer := &prometheusMetricObserver{
		prometheusClientFn: metricsclient.NewPrometheusClient,
		queryHandlers:      queryHandlers,
		operatorClient:     operatorClient,
		serviceClient:      serviceClient,
		secretsClient:      secretsClient,
		configsClient:      configsClient,
		routeClient:        routeClient,
	}
	return factory.New().ResyncEvery(defaultPollingDuration).WithSync(observer.sync).ToController(name, recorder.WithComponentSuffix(name))
}

func (o *prometheusMetricObserver) sync(ctx context.Context, controllerContext factory.SyncContext) error {
	var (
		errors          []error
		foundConditions []operatorv1.OperatorCondition
	)

	// list condition types we already know about
	degradedConditionNames := sets.NewString("PrometheusObserverDegraded")

	prometheusClient, prometheusClientErr := o.prometheusClientFn(o.serviceClient, o.secretsClient, o.configsClient, o.routeClient)
	// do not error out if we can't get client, but report all handler degraded status as "unknown".
	if prometheusClientErr != nil {
		klog.Infof("Failed to get prometheus client: %v", prometheusClientErr)
	}

	for _, handler := range o.queryHandlers {
		handlerConditionType := fmt.Sprintf("%s_PrometheusObserverDegraded", handler.Name)
		// register handler name as operator degraded condition
		degradedConditionNames.Insert(handlerConditionType)
		// if we were not able to acquire prometheus client (service does not exists yet, upgrade, etc.) lets report 'unknown' for this condition
		if prometheusClient == nil {
			foundConditions = append(foundConditions, operatorv1.OperatorCondition{
				Type:    handlerConditionType,
				Status:  operatorv1.ConditionUnknown,
				Reason:  "PrometheusUnavailable",
				Message: fmt.Sprintf("Prometheus client is not available: %v", prometheusClientErr),
			})
			continue
		}

		// pass the polling interval to handler so callers know how often we execute this handler
		handlerCtx := context.WithValue(ctx, "polling_interval", defaultPollingDuration)

		if err := handler.Handler(handlerCtx, controllerContext.Recorder(), prometheusClient); err != nil {
			// handler returning error means the operator should go degraded
			foundConditions = append(foundConditions, operatorv1.OperatorCondition{
				Type:    handlerConditionType,
				Status:  operatorv1.ConditionTrue,
				Reason:  "PrometheusQueryFailed",
				Message: err.Error(),
			})
		}
	}
	syncErr := errorutil.NewAggregate(errors)

	// if syncErr occurred, it means we failed to issue the prometheus query (connection problem) or the query result was nil (wrong query).
	// in both cases, we go degraded with unknown status as we can't process the handler.
	if syncErr != nil {
		foundConditions = append(foundConditions, operatorv1.OperatorCondition{
			Type:    "PrometheusObserverDegraded",
			Status:  operatorv1.ConditionUnknown,
			Reason:  "QueryFailed",
			Message: syncErr.Error(),
		})
	}

	var updateConditionFuncs []v1helpers.UpdateStaticPodStatusFunc
	// check the supported degraded foundConditions and check if any pending pod matching them.
	for _, degradedConditionName := range degradedConditionNames.List() {
		// clean up existing foundConditions
		updatedCondition := operatorv1.OperatorCondition{
			Type:   degradedConditionName,
			Status: operatorv1.ConditionFalse,
		}
		if condition := v1helpers.FindOperatorCondition(foundConditions, degradedConditionName); condition != nil {
			updatedCondition = *condition
		}
		updateConditionFuncs = append(updateConditionFuncs, v1helpers.UpdateStaticPodConditionFn(updatedCondition))
	}
	if _, _, err := v1helpers.UpdateStaticPodStatus(o.operatorClient, updateConditionFuncs...); err != nil {
		return err
	}

	return syncErr
}
