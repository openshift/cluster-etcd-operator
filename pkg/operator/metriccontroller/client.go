package metriccontroller

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	prometheusapi "github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
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

func getPrometheusClient(httpTransport *http.Transport) (prometheusv1.API, error) {
	saToken, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return nil, fmt.Errorf("error reading service account token: %w", err)
	}

	client, err := prometheusapi.NewClient(prometheusapi.Config{
		Address: "https://" + net.JoinHostPort("thanos-querier.openshift-monitoring.svc", "9091"),
		RoundTripper: transport.NewBearerAuthRoundTripper(
			string(saToken),
			httpTransport,
		),
	})
	if err != nil {
		return nil, fmt.Errorf("error creating prometheus client: %w", err)
	}

	return prometheusv1.NewAPI(client), nil
}
