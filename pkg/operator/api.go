package operator

// this file is only used to vendor CRD yaml

import (
	_ "github.com/openshift/api/config/v1alpha1/zz_generated.crd-manifests"

	_ "github.com/openshift/api/operator/v1alpha1"
	_ "github.com/openshift/api/operator/v1alpha1/zz_generated.crd-manifests"

	_ "github.com/openshift/api/operator/v1"
	_ "github.com/openshift/api/operator/v1/zz_generated.crd-manifests"
	// TODO(thomas): do we need to import the HWspeed feature separately?
	// operator/v1/zz_generated.featuregated-crd-manifests/etcds.operator.openshift.io/HardwareSpeed.yaml
)
