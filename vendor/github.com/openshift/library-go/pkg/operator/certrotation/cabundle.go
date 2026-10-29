package certrotation

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"reflect"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/cert"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	"github.com/openshift/library-go/pkg/certs"
	"github.com/openshift/library-go/pkg/crypto"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourcehelper"
)

// CABundleConfigMap maintains a CA bundle config map, by adding new CA certs coming from RotatedSigningCASecret, and by removing expired old ones.
type CABundleConfigMap struct {
	// Namespace is the namespace of the ConfigMap to maintain.
	namespace string
	// Name is the name of the ConfigMap to maintain.
	name string
	// RefreshOnlyWhenExpired set to true means to ignore 80% of validity and the Refresh duration for rotation,
	// but only rotate when the certificate expires. This is useful for auto-recovery when we want to enforce
	// rotation on expiration only, but not interfere with the ordinary rotation controller.
	refreshOnlyWhenExpired bool
	// Owner is an optional reference to add to the secret that this rotator creates.
	owner *metav1.OwnerReference
	// AdditionalAnnotations is a collection of annotations set for the secret
	additionalAnnotations AdditionalAnnotations
	// Plumbing:
	informer      corev1informers.ConfigMapInformer
	lister        corev1listers.ConfigMapLister
	client        corev1client.ConfigMapsGetter
	eventRecorder events.Recorder

	clock clock.PassiveClock
}

func NewCABundleConfigMap(
	namespace string,
	name string,
	refreshOnlyWhenExpired bool,
	owner *metav1.OwnerReference,
	additionalAnnotations AdditionalAnnotations,
	informer corev1informers.ConfigMapInformer,
	client corev1client.ConfigMapsGetter,
	eventRecorder events.Recorder,
) CABundleConfigMap {
	return CABundleConfigMap{
		name:                   name,
		namespace:              namespace,
		refreshOnlyWhenExpired: refreshOnlyWhenExpired,
		owner:                  owner,
		additionalAnnotations:  additionalAnnotations,
		informer:               informer,
		lister:                 informer.Lister(),
		client:                 client,
		eventRecorder:          eventRecorder,
		clock:                  clock.RealClock{},
	}
}

func (c CABundleConfigMap) EnsureConfigMapCABundle(ctx context.Context, signingCertKeyPair *crypto.CA, signingCertKeyPairLocation string) ([]*x509.Certificate, error) {
	// by this point we have current signing cert/key pair.  We now need to make sure that the ca-bundle configmap has this cert and
	// doesn't have any expired certs
	updateRequired := false
	creationRequired := false

	originalCABundleConfigMap, err := c.lister.ConfigMaps(c.namespace).Get(c.name)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	caBundleConfigMap := originalCABundleConfigMap.DeepCopy()
	if apierrors.IsNotFound(err) {
		// create an empty one
		caBundleConfigMap = &corev1.ConfigMap{ObjectMeta: NewTLSArtifactObjectMeta(
			c.name,
			c.namespace,
			c.additionalAnnotations,
		)}
		creationRequired = true
	}

	// run Update if metadata needs changing unless running in RefreshOnlyWhenExpired mode
	if !c.refreshOnlyWhenExpired {
		needsOwnerUpdate := false
		if c.owner != nil {
			needsOwnerUpdate = ensureOwnerReference(&caBundleConfigMap.ObjectMeta, c.owner)
		}
		needsMetadataUpdate := c.additionalAnnotations.EnsureTLSMetadataUpdate(&caBundleConfigMap.ObjectMeta)
		updateRequired = needsOwnerUpdate || needsMetadataUpdate
	}

	updatedCerts, err := manageCABundleConfigMap(caBundleConfigMap, signingCertKeyPair.Config.Certs[0], c.clock)
	if err != nil {
		return nil, err
	}
	if originalCABundleConfigMap == nil || originalCABundleConfigMap.Data == nil || !equality.Semantic.DeepEqual(originalCABundleConfigMap.Data, caBundleConfigMap.Data) {
		reason := ""
		if creationRequired {
			reason = "configmap doesn't exist"
		} else if originalCABundleConfigMap.Data == nil {
			reason = "configmap is empty"
		} else if !equality.Semantic.DeepEqual(originalCABundleConfigMap.Data, caBundleConfigMap.Data) {
			reason = fmt.Sprintf("signer update %s", signingCertKeyPairLocation)
		}
		c.eventRecorder.Eventf("CABundleUpdateRequired", "%q in %q requires a new cert: %s", c.name, c.namespace, reason)
		LabelAsManagedConfigMap(caBundleConfigMap, CertificateTypeCABundle)

		updateRequired = true
	}

	if creationRequired {
		actualCABundleConfigMap, err := c.client.ConfigMaps(c.namespace).Create(ctx, caBundleConfigMap, metav1.CreateOptions{})
		resourcehelper.ReportCreateEvent(c.eventRecorder, actualCABundleConfigMap, err)
		if err != nil {
			return nil, err
		}
		klog.V(2).Infof("Created ca-bundle.crt configmap %s/%s with:\n%s", certs.CertificateBundleToString(updatedCerts), caBundleConfigMap.Namespace, caBundleConfigMap.Name)
		caBundleConfigMap = actualCABundleConfigMap
	} else if updateRequired {
		actualCABundleConfigMap, err := c.client.ConfigMaps(c.namespace).Update(ctx, caBundleConfigMap, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			// ignore error if its attempting to update outdated version of the configmap
			return nil, nil
		}
		resourcehelper.ReportUpdateEvent(c.eventRecorder, actualCABundleConfigMap, err)
		if err != nil {
			return nil, err
		}
		klog.V(2).Infof("Updated ca-bundle.crt configmap %s/%s with:\n%s", certs.CertificateBundleToString(updatedCerts), caBundleConfigMap.Namespace, caBundleConfigMap.Name)
		caBundleConfigMap = actualCABundleConfigMap
	}

	caBundle := caBundleConfigMap.Data["ca-bundle.crt"]
	if len(caBundle) == 0 {
		return nil, fmt.Errorf("configmap/%s -n%s missing ca-bundle.crt", caBundleConfigMap.Name, caBundleConfigMap.Namespace)
	}
	certificates, err := cert.ParseCertsPEM([]byte(caBundle))
	if err != nil {
		return nil, err
	}

	return certificates, nil
}

// manageCABundleConfigMap adds the new certificate to the list of cabundles, eliminates duplicates, and prunes the list of expired
// certs to trust as signers
func manageCABundleConfigMap(caBundleConfigMap *corev1.ConfigMap, currentSigner *x509.Certificate, clock clock.PassiveClock) ([]*x509.Certificate, error) {
	if caBundleConfigMap.Data == nil {
		caBundleConfigMap.Data = map[string]string{}
	}

	certificates := []*x509.Certificate{}
	caBundle := caBundleConfigMap.Data["ca-bundle.crt"]
	if len(caBundle) > 0 {
		var err error
		certificates, err = cert.ParseCertsPEM([]byte(caBundle))
		if err != nil {
			return nil, err
		}
	}
	certificates = append([]*x509.Certificate{currentSigner}, certificates...)
	certificates = crypto.FilterExpiredCertsWithClock(clock, certificates...)

	finalCertificates := []*x509.Certificate{}
	// now check for duplicates. n^2, but super simple
	for i := range certificates {
		found := false
		for j := range finalCertificates {
			if reflect.DeepEqual(certificates[i].Raw, finalCertificates[j].Raw) {
				found = true
				break
			}
		}
		if !found {
			finalCertificates = append(finalCertificates, certificates[i])
		}
	}

	// sorting ensures we don't continuously swap the certificates in the bundle, which might cause revision rollouts
	sort.SliceStable(finalCertificates, func(i, j int) bool {
		return bytes.Compare(finalCertificates[i].Raw, finalCertificates[j].Raw) < 0
	})
	caBytes, err := crypto.EncodeCertificates(finalCertificates...)
	if err != nil {
		return nil, err
	}

	caBundleConfigMap.Data["ca-bundle.crt"] = string(caBytes)

	return finalCertificates, nil
}
