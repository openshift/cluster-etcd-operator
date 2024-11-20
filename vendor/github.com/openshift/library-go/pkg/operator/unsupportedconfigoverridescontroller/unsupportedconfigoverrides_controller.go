package unsupportedconfigoverridescontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/klog/v2"

	operatorv1 "github.com/openshift/api/operator/v1"

	applyoperatorv1 "github.com/openshift/client-go/operator/applyconfigurations/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/condition"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
)

// UnsupportedConfigOverridesController is a controller that will copy source configmaps and secrets to their destinations.
// It will also mirror deletions by deleting destinations.
type UnsupportedConfigOverridesController struct {
	controllerInstanceName string
	operatorClient         v1helpers.OperatorClient
}

// NewUnsupportedConfigOverridesController creates UnsupportedConfigOverridesController.
func NewUnsupportedConfigOverridesController(
	instanceName string,
	operatorClient v1helpers.OperatorClient,
	eventRecorder events.Recorder,
) factory.Controller {
	c := &UnsupportedConfigOverridesController{
		controllerInstanceName: factory.ControllerInstanceName(instanceName, "UnsupportedConfigOverrides"),
		operatorClient:         operatorClient,
	}
	return factory.New().
		WithInformers(operatorClient.Informer()).
		WithSync(c.sync).
		ToController(
			c.controllerInstanceName,
			eventRecorder,
		)
}

func (c *UnsupportedConfigOverridesController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	operatorSpec, _, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}

	if !management.IsOperatorManaged(operatorSpec.ManagementState) {
		return nil
	}

	cond := applyoperatorv1.OperatorCondition().
		WithType(condition.UnsupportedConfigOverridesUpgradeableConditionType).
		WithStatus(operatorv1.ConditionTrue).
		WithReason("NoUnsupportedConfigOverrides")

	if len(operatorSpec.UnsupportedConfigOverrides.Raw) > 0 {
		cond = cond.
			WithStatus(operatorv1.ConditionFalse).
			WithReason("UnsupportedConfigOverridesSet").
			WithMessage(fmt.Sprintf("unsupportedConfigOverrides=%v", string(operatorSpec.UnsupportedConfigOverrides.Raw)))

		// try to get a prettier message
		keys, err := keysSetInUnsupportedConfig(operatorSpec.UnsupportedConfigOverrides.Raw)
		if err == nil {
			cond = cond.WithMessage(fmt.Sprintf("setting: %v", sets.List(keys)))
		}
	}

	return c.operatorClient.ApplyOperatorStatus(
		ctx,
		c.controllerInstanceName,
		applyoperatorv1.OperatorStatus().WithConditions(cond),
	)
}

func keysSetInUnsupportedConfig(configYaml []byte) (sets.Set[string], error) {
	configJson, err := kyaml.ToJSON(configYaml)
	if err != nil {
		klog.Warning(err)
		// maybe it's just json
		configJson = configYaml
	}

	config := map[string]interface{}{}
	if err := json.NewDecoder(bytes.NewBuffer(configJson)).Decode(&config); err != nil {
		return nil, err
	}

	return keysSetInUnsupportedConfigMap([]string{}, config), nil
}

func keysSetInUnsupportedConfigMap(pathSoFar []string, config map[string]interface{}) sets.Set[string] {
	ret := sets.Set[string]{}

	for k, v := range config {
		currPath := append(pathSoFar, k)

		switch castV := v.(type) {
		case map[string]interface{}:
			ret.Insert(keysSetInUnsupportedConfigMap(currPath, castV).UnsortedList()...)
		case []interface{}:
			ret.Insert(keysSetInUnsupportedConfigSlice(currPath, castV).UnsortedList()...)
		default:
			ret.Insert(strings.Join(currPath, "."))
		}
	}

	return ret
}

func keysSetInUnsupportedConfigSlice(pathSoFar []string, config []interface{}) sets.Set[string] {
	ret := sets.Set[string]{}

	for index, v := range config {
		currPath := append(pathSoFar, fmt.Sprintf("%d", index))

		switch castV := v.(type) {
		case map[string]interface{}:
			ret.Insert(keysSetInUnsupportedConfigMap(currPath, castV).UnsortedList()...)
		case []interface{}:
			ret.Insert(keysSetInUnsupportedConfigSlice(currPath, castV).UnsortedList()...)
		default:
			ret.Insert(strings.Join(currPath, "."))
		}
	}

	return ret
}
