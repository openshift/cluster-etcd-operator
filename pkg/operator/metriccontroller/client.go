package metriccontroller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	prometheusapi "github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/transport"
)

func getTransport() (*http.Transport, error) {
	serviceCABytes, err := ioutil.ReadFile("/var/run/configmaps/etcd-service-ca/service-ca.crt")
	if err != nil {
		return nil, err
	}

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(serviceCABytes)

	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			RootCAs: roots,
		},
	}, nil
}

func getPrometheusClient(ctx context.Context, secretClient coreclientv1.SecretsGetter, httpTransport *http.Transport) (prometheusv1.API, error) {
	secrets, err := secretClient.Secrets("openshift-monitoring").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	bearerToken := ""
	for _, s := range secrets.Items {
		if s.Type != corev1.SecretTypeServiceAccountToken ||
			!strings.HasPrefix(s.Name, "prometheus-k8s") {
			continue
		}
		bearerToken = string(s.Data[corev1.ServiceAccountTokenKey])
		break
	}
	if len(bearerToken) == 0 {
		return nil, fmt.Errorf("unable to retrieve prometheus-k8 bearer token")
	}

	client, err := prometheusapi.NewClient(prometheusapi.Config{
		Address: "https://" + net.JoinHostPort("thanos-querier.openshift-monitoring.svc", "9091"),
		RoundTripper: transport.NewBearerAuthRoundTripper(
			bearerToken,
			httpTransport,
		),
	})

	return prometheusv1.NewAPI(client), nil
}
