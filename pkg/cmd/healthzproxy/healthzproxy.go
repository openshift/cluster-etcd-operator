package healthzproxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"
)

// healthzOpts holds values to drive the healthz proxy.
type healthzOpts struct {
	etcdEndpoint string

	// TLS auth for proxy client
	clientCert   string
	clientKey    string
	clientCACert string

	// TLS for /healthz handler
	listenPort uint16
	listenCert string
	listenKey  string
}

// NewHealthzProxyCommand creates a healthz-proxy command.
func NewHealthzProxyCommand() *cobra.Command {
	opts := healthzOpts{
		listenPort:   10100, // TODO random for testing need to register with proper authorities
		etcdEndpoint: "https://localhost:2379",
	}
	cmd := &cobra.Command{
		Use:   "healthz-proxy",
		Short: "Proxy TCP /health check to etcd server without TLS authentication",
		Run: func(cmd *cobra.Command, args []string) {
			if err := opts.Validate(); err != nil {
				klog.Fatal(err)
			}
			if err := opts.Run(); err != nil {
				klog.Fatal(err)
			}
		},
	}

	opts.AddFlags(cmd.Flags())

	return cmd
}

func (h *healthzOpts) AddFlags(fs *pflag.FlagSet) {
	fs.Uint16Var(&h.listenPort, "listen-port", h.listenPort, "Listen on this port")
	fs.StringVar(&h.listenCert, "listen-cert", "", "secure connections to the proxy using this TLS certificate file")
	fs.StringVar(&h.listenKey, "listen-key", "", "secure connections to the proxy using this TLS key file")
	fs.StringVar(&h.clientCert, "client-cert", "", "proxy client requests to etcd use this TLS certificate file")
	fs.StringVar(&h.clientKey, "client-key", "", "proxy client requests to etcd use this TLS key file")
	fs.StringVar(&h.clientCACert, "client-cacert", "", "proxy client requests to etcd use this TLS CA certificate file")
	fs.StringVar(&h.etcdEndpoint, "etcd-endpoint", h.etcdEndpoint, "proxy client will dial this etcd endpoint")
}

// Validate verifies the inputs.
func (r *healthzOpts) Validate() error {
	if r.listenPort == 0 {
		return fmt.Errorf("--listen-port must be between 1 and 65535")
	}
	if r.listenKey == "" {
		return fmt.Errorf("missing required flag: --listen-key")
	}
	if r.listenCert == "" {
		return fmt.Errorf("missing required flag: --listen-cert")
	}
	if r.clientKey == "" {
		return fmt.Errorf("missing required flag: --client-key")
	}
	if r.clientCert == "" {
		return fmt.Errorf("missing required flag: --client-cert")
	}
	if r.clientCACert == "" {
		return fmt.Errorf("missing required flag: --client-cacert")
	}
	if r.etcdEndpoint == "" {
		return fmt.Errorf("missing required flag: --etcd-endpoint")
	}
	return nil
}

type Health struct {
	Health string `json:"health"`
}

// Run contains the logic of the insecure-readyz command.
func (h *healthzOpts) Run() error {
	caCert, err := ioutil.ReadFile(h.clientCACert)
	if err != nil {
		return err
	}
	keyPair, err := tls.LoadX509KeyPair(h.clientCert, h.clientKey)
	if err != nil {
		return err
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{keyPair},
		RootCAs:      caPool,
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: tlsConfig,
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
		resp, err := client.Get(h.etcdEndpoint + "/health")
		if err != nil {
			http.Error(w, "couldn't contact etcd", http.StatusInternalServerError)
			klog.Warningf("Failed to get %q: %v", "/health", err)
			return
		}
		defer resp.Body.Close()
		var check Health
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, "failed to read response from etcd health check", http.StatusInternalServerError)
			klog.Warningf("Failed to read the response body: %v", err)
			return
		}
		json.Unmarshal(body, &check)

		if check.Health == "true" {
			w.WriteHeader(http.StatusOK)
		} else {
			klog.Warningf("health check failed: %s", string(body))
			w.WriteHeader(http.StatusBadRequest)
		}

		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.Write(body)
	})

	addr := fmt.Sprintf("0.0.0.0:%d", h.listenPort)
	klog.Infof("Listening on %s", addr)
	return http.ListenAndServeTLS(addr, h.listenCert, h.listenKey, mux)
}
