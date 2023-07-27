package backuphelpers

import (
	"fmt"
	configExt "github.com/openshift/client-go/config/informers/externalversions"
	operatorExt "github.com/openshift/client-go/operator/informers/externalversions"
	"github.com/openshift/cluster-etcd-operator/pkg/operator/ceohelpers"
	"k8s.io/client-go/tools/cache"

	"github.com/openshift/library-go/pkg/operator/configobserver/featuregates"
)

const AutomatedEtcdBackupFeatureGateName = "AutomatedEtcdBackup"

func AutoBackupFeatureGateEnabled(featureGateAccessor featuregates.FeatureGateAccess) (bool, error) {
	gates, err := featureGateAccessor.CurrentFeatureGates()
	if err != nil {
		return false, fmt.Errorf("could not access feature gates, error was: %w", err)
	}

	return gates.Enabled(AutomatedEtcdBackupFeatureGateName), nil
}

func CreateBackupInformers(
	featureGateAccessor featuregates.FeatureGateAccess,
	operatorInformers operatorExt.SharedInformerFactory,
	configInformers configExt.SharedInformerFactory) (cache.SharedInformer, cache.SharedInformer, error) {

	enabledAutoBackupFeature, err := AutoBackupFeatureGateEnabled(featureGateAccessor)
	if err != nil {
		return nil, nil, fmt.Errorf("could not determine AutoBackupFeatureGateEnabled, aborting controller start: %w", err)
	}

	etcdBackupInformer := operatorInformers.Operator().V1alpha1().EtcdBackups().Informer()
	configBackupInformer := configInformers.Config().V1alpha1().Backups().Informer()
	// in order for us to not block controller startup on APIs that do not exist (yet), we're going to inject an already synchronized informer
	if !enabledAutoBackupFeature {
		return ceohelpers.NewAlreadySyncedInformer(etcdBackupInformer), ceohelpers.NewAlreadySyncedInformer(configBackupInformer), nil
	}

	return etcdBackupInformer, configBackupInformer, nil
}
