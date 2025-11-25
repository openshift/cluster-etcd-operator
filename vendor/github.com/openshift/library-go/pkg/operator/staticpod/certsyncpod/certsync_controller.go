package certsyncpod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/openshift/library-go/pkg/operator/staticpod/internal/atomicdir/types"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1interface "k8s.io/client-go/kubernetes/typed/core/v1"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/installer"
	"github.com/openshift/library-go/pkg/operator/staticpod/internal/atomicdir"
)

const stagingDirUID = "cert-sync"

type CertSyncController struct {
	destinationDir string
	namespace      string
	configMaps     []installer.UnrevisionedResource
	secrets        []installer.UnrevisionedResource

	configMapGetter corev1interface.ConfigMapInterface
	configMapLister v1.ConfigMapLister
	secretGetter    corev1interface.SecretInterface
	secretLister    v1.SecretLister
	eventRecorder   events.Recorder
}

func NewCertSyncController(targetDir, targetNamespace string, configmaps, secrets []installer.UnrevisionedResource, kubeClient kubernetes.Interface, informers informers.SharedInformerFactory, eventRecorder events.Recorder) factory.Controller {
	c := &CertSyncController{
		destinationDir: targetDir,
		namespace:      targetNamespace,
		configMaps:     configmaps,
		secrets:        secrets,
		eventRecorder:  eventRecorder.WithComponentSuffix("cert-sync-controller"),

		configMapGetter: kubeClient.CoreV1().ConfigMaps(targetNamespace),
		configMapLister: informers.Core().V1().ConfigMaps().Lister(),
		secretGetter:    kubeClient.CoreV1().Secrets(targetNamespace),
		secretLister:    informers.Core().V1().Secrets().Lister(),
	}

	return factory.New().
		WithInformers(
			informers.Core().V1().ConfigMaps().Informer(),
			informers.Core().V1().Secrets().Informer(),
		).
		WithSync(c.sync).
		ToController(
			"CertSyncController", // don't change what is passed here unless you also remove the old FooDegraded condition
			eventRecorder,
		)
}

func (c *CertSyncController) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	if err := os.RemoveAll(getStagingDir(c.destinationDir)); err != nil {
		c.eventRecorder.Warningf("CertificateUpdateFailed", fmt.Sprintf("Failed to prune staging directory: %v", err))
		return err
	}

	errors := []error{}

	klog.Infof("Syncing configmaps: %v", c.configMaps)
	for _, cm := range c.configMaps {
		configMap, err := c.configMapLister.ConfigMaps(c.namespace).Get(cm.Name)
		switch {
		case apierrors.IsNotFound(err) && !cm.Optional:
			errors = append(errors, err)
			continue

		case apierrors.IsNotFound(err) && cm.Optional:
			configMapFile := getConfigMapTargetDir(c.destinationDir, cm.Name)
			if _, err := os.Stat(configMapFile); os.IsNotExist(err) {
				// if the configmap file does not exist, there is no work to do, so skip making any live check and just return.
				// if the configmap actually exists in the API, we'll eventually see it on the watch.
				continue
			}

			// Check with the live call it is really missing
			configMap, err = c.configMapGetter.Get(ctx, cm.Name, metav1.GetOptions{})
			if err == nil {
				klog.Infof("Caches are stale. They don't see configmap '%s/%s', yet it is present", configMap.Namespace, configMap.Name)
				// We will get re-queued when we observe the change
				continue
			}
			if !apierrors.IsNotFound(err) {
				errors = append(errors, err)
				continue
			}

			// remove missing content
			if err := os.RemoveAll(configMapFile); err != nil {
				c.eventRecorder.Warningf("CertificateUpdateFailed", "Failed removing file for configmap: %s/%s: %v", c.namespace, cm.Name, err)
				errors = append(errors, err)
			}
			c.eventRecorder.Eventf("CertificateRemoved", "Removed file for configmap: %s/%s", c.namespace, cm.Name)
			continue

		case err != nil:
			c.eventRecorder.Warningf("CertificateUpdateFailed", "Failed getting configmap: %s/%s: %v", c.namespace, cm.Name, err)
			errors = append(errors, err)
			continue
		}

		contentDir := getConfigMapTargetDir(c.destinationDir, cm.Name)
		stagingDir := getConfigMapStagingDir(c.destinationDir, cm.Name)

		data := make(map[string]string, len(configMap.Data))
		for filename := range configMap.Data {
			fullFilename := filepath.Join(contentDir, filename)

			existingContent, err := os.ReadFile(fullFilename)
			if err != nil {
				if !os.IsNotExist(err) {
					klog.Error(err)
				}
				continue
			}

			data[filename] = string(existingContent)
		}

		// Check if cached configmap differs
		if reflect.DeepEqual(configMap.Data, data) {
			continue
		}

		klog.V(2).Infof("Syncing updated configmap '%s/%s'.", configMap.Namespace, configMap.Name)

		// We need to do a live get here so we don't overwrite a newer file with one from a stale cache
		configMap, err = c.configMapGetter.Get(ctx, configMap.Name, metav1.GetOptions{})
		if err != nil {
			// Even if the error is not exists we will act on it when caches catch up
			c.eventRecorder.Warningf("CertificateUpdateFailed", "Failed getting configmap: %s/%s: %v", c.namespace, cm.Name, err)
			errors = append(errors, err)
			continue
		}

		// Check if the live configmap differs
		if reflect.DeepEqual(configMap.Data, data) {
			klog.Infof("Caches are stale. The live configmap '%s/%s' is reflected on filesystem, but cached one differs", configMap.Namespace, configMap.Name)
			continue
		}

		files := make(map[string][]byte, len(configMap.Data))
		for k, v := range configMap.Data {
			files[k] = []byte(v)
		}
		errors = append(errors, syncDirectory(c.eventRecorder, "configmap", configMap.ObjectMeta, contentDir, 0755, stagingDir, files, 0644))
	}

	klog.Infof("Syncing secrets: %v", c.secrets)
	for _, s := range c.secrets {
		secret, err := c.secretLister.Secrets(c.namespace).Get(s.Name)
		switch {
		case apierrors.IsNotFound(err) && !s.Optional:
			errors = append(errors, err)
			continue

		case apierrors.IsNotFound(err) && s.Optional:
			secretFile := getSecretTargetDir(c.destinationDir, s.Name)
			if _, err := os.Stat(secretFile); os.IsNotExist(err) {
				// if the secret file does not exist, there is no work to do, so skip making any live check and just return.
				// if the secret actually exists in the API, we'll eventually see it on the watch.
				continue
			}

			// Check with the live call it is really missing
			secret, err = c.secretGetter.Get(ctx, s.Name, metav1.GetOptions{})
			if err == nil {
				klog.Infof("Caches are stale. They don't see secret '%s/%s', yet it is present", secret.Namespace, secret.Name)
				// We will get re-queued when we observe the change
				continue
			}
			if !apierrors.IsNotFound(err) {
				errors = append(errors, err)
				continue
			}

			// remove missing content
			if err := os.RemoveAll(secretFile); err != nil {
				c.eventRecorder.Warningf("CertificateUpdateFailed", "Failed removing file for missing secret: %s/%s: %v", c.namespace, s.Name, err)
				errors = append(errors, err)
				continue
			}
			c.eventRecorder.Warningf("CertificateRemoved", "Removed file for missing secret: %s/%s", c.namespace, s.Name)
			continue

		case err != nil:
			c.eventRecorder.Warningf("CertificateUpdateFailed", "Failed getting secret: %s/%s: %v", c.namespace, s.Name, err)
			errors = append(errors, err)
			continue
		}

		contentDir := getSecretTargetDir(c.destinationDir, s.Name)
		stagingDir := getSecretStagingDir(c.destinationDir, s.Name)

		data := make(map[string][]byte, len(secret.Data))
		for filename := range secret.Data {
			fullFilename := filepath.Join(contentDir, filename)

			existingContent, err := os.ReadFile(fullFilename)
			if err != nil {
				if !os.IsNotExist(err) {
					klog.Error(err)
				}
				continue
			}

			data[filename] = existingContent
		}

		// Check if cached secret differs
		if reflect.DeepEqual(secret.Data, data) {
			continue
		}

		klog.V(2).Infof("Syncing updated secret '%s/%s'.", secret.Namespace, secret.Name)

		// We need to do a live get here so we don't overwrite a newer file with one from a stale cache
		secret, err = c.secretGetter.Get(ctx, secret.Name, metav1.GetOptions{})
		if err != nil {
			// Even if the error is not exists we will act on it when caches catch up
			c.eventRecorder.Warningf("CertificateUpdateFailed", "Failed getting secret: %s/%s: %v", c.namespace, s.Name, err)
			errors = append(errors, err)
			continue
		}

		// Check if the live secret differs
		if reflect.DeepEqual(secret.Data, data) {
			klog.Infof("Caches are stale. The live secret '%s/%s' is reflected on filesystem, but cached one differs", secret.Namespace, secret.Name)
			continue
		}

		errors = append(errors, syncDirectory(c.eventRecorder, "secret", secret.ObjectMeta, contentDir, 0755, stagingDir, secret.Data, 0600))
	}
	return utilerrors.NewAggregate(errors)
}

func syncDirectory(
	eventRecorder events.Recorder,
	typeName string, o metav1.ObjectMeta,
	targetDir string, targetDirPerm os.FileMode, stagingDir string,
	fileContents map[string][]byte, filePerm os.FileMode,
) error {
	files := make(map[string]types.File, len(fileContents))
	for filename, content := range fileContents {
		files[filename] = types.File{
			Content: content,
			Perm:    filePerm,
		}
	}

	if err := atomicdir.Sync(targetDir, targetDirPerm, stagingDir, files); err != nil {
		err = fmt.Errorf("failed to sync %s %s/%s (directory %q): %w", typeName, o.Name, o.Namespace, targetDir, err)
		eventRecorder.Warning("CertificateUpdateFailed", err.Error())
		return err
	}
	eventRecorder.Eventf("CertificateUpdated", "Wrote updated %s: %s/%s", typeName, o.Namespace, o.Name)
	return nil
}

func getStagingDir(targetDir string) string {
	return filepath.Join(targetDir, "staging", stagingDirUID)
}

func getConfigMapTargetDir(targetDir, configMapName string) string {
	return filepath.Join(targetDir, "configmaps", configMapName)
}

func getConfigMapStagingDir(targetDir, configMapName string) string {
	return filepath.Join(getStagingDir(targetDir), "configmaps", configMapName)
}

func getSecretTargetDir(targetDir, secretName string) string {
	return filepath.Join(targetDir, "secrets", secretName)
}

func getSecretStagingDir(targetDir, secretName string) string {
	return filepath.Join(getStagingDir(targetDir), "secrets", secretName)
}
