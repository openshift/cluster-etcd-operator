package certsigner

import (
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/cfssl/config"
	"github.com/cloudflare/cfssl/helpers"
	"github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/signer"
	"github.com/cloudflare/cfssl/signer/local"
	"github.com/gorilla/mux"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	capi "k8s.io/api/certificates/v1beta1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog"
)

const (
	etcdPeer   = "EtcdPeer"
	etcdServer = "EtcdServer"
	etcdMetric = "EtcdMetric"
)

var (
	// defaultCertDuration is initialized to 365 days
	defaultCertDuration = 24 * 365 * time.Hour
	// ErrInvalidOrg defines a global error for invalid organization
	ErrInvalidOrg = errors.New("invalid organization")
	// ErrInvalidCN defines a global error for invalid subject common name
	ErrInvalidCN = errors.New("invalid subject Common Name")
	// ErrProfileSupport defines a global error for a profile which was not backed by a CA signer cert..
	ErrProfileSupport = errors.New("csr profile is not currently supported")
)

// CertServer is the object that handles the HTTP requests and responses.
// It recieves CSR approval requests from the client agent which the `signer`
// then attempts to sign. If successful, the approved CSR is returned to the
// agent which contains the signed certificate.
type CertServer struct {
	// mux is a request router instance
	mux *mux.Router
	// csrDir is the directory location where the signer stores CSRs
	csrDir string
	// signer is the object that handles the approval of the CSRs
	signer *CertSigner
	// policy
	policy *config.Signing
	// caFiles
	caFiles *SignerCAFiles
}

// CertSigner signs a certiifcate using a `cfssl` Signer.
//
// NOTE: the CertSigner only signs certificates for `etcd` nodes, any other
// certificate request from other nodes will be declined.
type CertSigner struct {
	// caCert is the x509 PEM encoded certifcate of the CA used for the
	// cfssl signer
	caCert *x509.Certificate
	// caCert is the x509 PEM encoded private key of the CA used for the
	// cfssl signer
	caKey crypto.Signer
	// cfsslSigner is a `cfssl` Signer that can sign a certificate based on a
	// certificate request.
	cfsslSigner *local.Signer
}

// CertKey stores files for the cert and key pair.
type CertKey struct {
	CertFile, KeyFile string
}

// Config holds the configuration values required to start a new signer
type Config struct {
	// SignerCAFiles
	SignerCAFiles
	// ServerCertKeys is a list of server certificates for serving on TLS based on SNI
	ServerCertKeys []CertKey
	// ListenAddress is the address at which the server listens for requests
	ListenAddress string
	// EtcdMetricCertDuration
	EtcdMetricCertDuration time.Duration
	// EtcdPeerCertDuration is the cert duration for the `EtcdPeer` profile
	EtcdPeerCertDuration time.Duration
	// EtcdServerCertDuration is the cert duration for the `EtcdServer` profile
	EtcdServerCertDuration time.Duration
	// CSRDir is the directory location where the signer stores CSRs and serves them
	CSRDir string
}

// SignerCAFiles holds the file paths to the signer CA assets
type SignerCAFiles struct {
	// CACert is the file location of the Certificate Authority certificate
	CACert string
	// CAKey is the file location of the Certificate Authority private key
	CAKey string
	// MetricCACert is the file location of the metrics Certificate Authority certificate
	MetricCACert string
	// MetricCAKey is the file location of the metrics Certificate Authority private key
	MetricCAKey string
}

// SignerCA stores the PEM encoded cert and key blocks.
type SignerCA struct {
	// caCert is the x509 PEM encoded certificate of the CA used for the
	// cfssl signer
	caCert *x509.Certificate
	// caCert is the x509 PEM encoded private key of the CA used for the
	// cfssl signer
	caKey crypto.Signer
}

// loggingHandler is the HTTP handler that logs information about requests received by the server
type loggingHandler struct {
	h http.Handler
}

func (l *loggingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Info(r.Method, r.URL.Path)
	l.h.ServeHTTP(w, r)
}

type serveOpts struct {
	caCrtFile     string
	caKeyFile     string
	mCACrtFile    string
	mCAKeyFile    string
	mCASigner     bool
	sCrtFiles     []string
	sKeyFiles     []string
	addr          string
	peerCertDur   string
	serverCertDur string
	metricCertDur string
	csrDir        string
	errOut        io.Writer
}

// NewCertSignerCommand creates an etcd cert signer server.
func NewCertSignerCommand(errOut io.Writer) *cobra.Command {
	serveOpts := &serveOpts{
		errOut: errOut,
	}
	cmd := &cobra.Command{
		Use:   "certsigner",
		Short: "serve cert signer server",
		Run: func(cmd *cobra.Command, args []string) {
			must := func(fn func() error) {
				if err := fn(); err != nil {
					if cmd.HasParent() {
						klog.Fatal(err)
					}
					fmt.Fprint(serveOpts.errOut, err.Error())
				}
			}

			must(serveOpts.Validate)
			must(serveOpts.Run)
		},
	}

	serveOpts.AddFlags(cmd.Flags())

	return cmd
}

func (s *serveOpts) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&s.caCrtFile, "cacrt", "", "CA certificate file for signer")
	fs.StringVar(&s.caKeyFile, "cakey", "", "CA private key file for signer")
	fs.StringArrayVar(&s.sCrtFiles, "servcrt", []string{}, "Server certificate file for signer")
	fs.StringArrayVar(&s.sKeyFiles, "servkey", []string{}, "Server private key file for signer")
	fs.StringVar(&s.mCACrtFile, "metric-cacrt", "", "CA certificate file for metrics signer")
	fs.StringVar(&s.mCAKeyFile, "metric-cakey", "", "CA private key file for metrics signer")
	fs.StringVar(&s.addr, "address", "0.0.0.0:6443", "Address on which the signer listens for requests")
	fs.StringVar(&s.metricCertDur, "metriccertdur", "8760h", "Certificate duration for etcd metrics certs (defaults to 365 days)")
	fs.StringVar(&s.peerCertDur, "peercertdur", "8760h", "Certificate duration for etcd peer certs (defaults to 365 days)")
	fs.StringVar(&s.serverCertDur, "servercertdur", "8760h", "Certificate duration for etcd server certs (defaults to 365 days)")
	fs.StringVar(&s.csrDir, "csrdir", "", "Directory location where signer will save CSRs.")
}

// Validate verifies the inputs.
func (s *serveOpts) Validate() error {
	caPair := 0
	if s.caCrtFile != "" && s.caKeyFile != "" {
		caPair++
	}
	if s.mCACrtFile != "" && s.mCAKeyFile != "" {
		caPair++
	}
	if caPair == 0 {
		return errors.New("no signer CA flags passed one cert/key pair is required")
	}

	if cl, kl := len(s.sCrtFiles), len(s.sKeyFiles); cl == 0 || kl == 0 {
		return errors.New("at least one pair of --servcrt and --servkey is required")
	} else if cl != kl {
		return fmt.Errorf("%d --servercrt does not match %d --servkey", cl, kl)
	}
	if s.csrDir == "" {
		return errors.New("missing required flag: --csrdir")
	}
	return nil
}

func (s *serveOpts) Run() error {
	pCertDur, err := time.ParseDuration(s.peerCertDur)
	if err != nil {
		return fmt.Errorf("error parsing duration for etcd peer cert: %v", err)
	}

	sCertDur, err := time.ParseDuration(s.serverCertDur)
	if err != nil {
		return fmt.Errorf("error parsing duration for etcd server cert: %v", err)
	}
	mCertDur, err := time.ParseDuration(s.metricCertDur)
	if err != nil {
		return fmt.Errorf("error parsing duration for etcd metric cert: %v", err)
	}

	ca := SignerCAFiles{
		CACert:       s.caCrtFile,
		CAKey:        s.caKeyFile,
		MetricCACert: s.mCACrtFile,
		MetricCAKey:  s.mCAKeyFile,
	}
	servercerts := make([]CertKey, len(s.sCrtFiles))
	for idx := range s.sCrtFiles {
		servercerts[idx] = CertKey{CertFile: s.sCrtFiles[idx], KeyFile: s.sKeyFiles[idx]}
	}
	c := Config{
		SignerCAFiles:          ca,
		ServerCertKeys:         servercerts,
		ListenAddress:          s.addr,
		EtcdMetricCertDuration: mCertDur,
		EtcdPeerCertDuration:   pCertDur,
		EtcdServerCertDuration: sCertDur,
		CSRDir:                 s.csrDir,
	}

	if err := StartSignerServer(c); err != nil {
		return fmt.Errorf("error starting signer: %v", err)
	}

	return nil
}

// NewServer returns a CertServer object that has a CertSigner object
// as a part of it
func NewServer(c Config) (*CertServer, error) {
	policy := signerPolicy(c)
	mux := mux.NewRouter()
	server := &CertServer{
		mux:    mux,
		csrDir: c.CSRDir,
		policy: &policy,

		caFiles: &c.SignerCAFiles,
	}

	mux.HandleFunc("/apis/certificates.k8s.io/v1beta1/certificatesigningrequests", server.HandlePostCSR).Methods("POST")
	mux.HandleFunc("/apis/certificates.k8s.io/v1beta1/certificatesigningrequests/{csrName}", server.HandleGetCSR).Methods("GET")

	return server, nil
}

func (s *CertServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// newSignerCA returns a SignerCA object of PEM encoded CA cert and keys based on the profile passed.
func newSignerCA(sc *SignerCAFiles, csr *capi.CertificateSigningRequest) (*SignerCA, error) {
	var caCert, caKey string

	profile, err := getProfile(csr)
	if err != nil {
		return nil, err
	}
	switch profile {
	case "EtcdMetric":
		if sc.MetricCAKey != "" && sc.MetricCACert != "" {
			caCert = sc.MetricCACert
			caKey = sc.MetricCAKey
			break
		}
		return nil, ErrProfileSupport
	case "EtcdServer", "EtcdPeer":
		if sc.CAKey != "" && sc.CACert != "" {
			caCert = sc.CACert
			caKey = sc.CAKey
			break
		}
		return nil, ErrProfileSupport
	default:
		return nil, ErrInvalidOrg
	}

	ca, err := ioutil.ReadFile(caCert)
	if err != nil {
		return nil, fmt.Errorf("error reading CA cert file %q: %v", caCert, err)
	}
	cakey, err := ioutil.ReadFile(caKey)
	if err != nil {
		return nil, fmt.Errorf("error reading CA key file %q: %v", caKey, err)
	}
	parsedCA, err := helpers.ParseCertificatePEM(ca)
	if err != nil {
		return nil, fmt.Errorf("error parsing CA cert file %q: %v", caCert, err)
	}
	privateKey, err := helpers.ParsePrivateKeyPEM(cakey)
	if err != nil {
		return nil, fmt.Errorf("Malformed private key %v", err)
	}

	return &SignerCA{
		caCert: parsedCA,
		caKey:  privateKey,
	}, nil
}

// signerPolicy
func signerPolicy(c Config) config.Signing {
	policy := config.Signing{
		Profiles: map[string]*config.SigningProfile{
			etcdPeer: &config.SigningProfile{
				Usage: []string{
					string(capi.UsageKeyEncipherment),
					string(capi.UsageDigitalSignature),
					string(capi.UsageClientAuth),
					string(capi.UsageServerAuth),
				},
				Expiry:       c.EtcdPeerCertDuration,
				ExpiryString: c.EtcdPeerCertDuration.String(),
			},
			etcdServer: &config.SigningProfile{
				Usage: []string{
					string(capi.UsageKeyEncipherment),
					string(capi.UsageDigitalSignature),
					string(capi.UsageClientAuth),
					string(capi.UsageServerAuth),
				},
				Expiry:       c.EtcdServerCertDuration,
				ExpiryString: c.EtcdServerCertDuration.String(),
			},
			etcdMetric: &config.SigningProfile{
				Usage: []string{
					string(capi.UsageKeyEncipherment),
					string(capi.UsageDigitalSignature),
					string(capi.UsageClientAuth),
					string(capi.UsageServerAuth),
				},
				Expiry:       c.EtcdMetricCertDuration,
				ExpiryString: c.EtcdMetricCertDuration.String(),
			},
		},
		Default: &config.SigningProfile{
			Usage: []string{
				string(capi.UsageKeyEncipherment),
				string(capi.UsageDigitalSignature),
			},
			Expiry:       defaultCertDuration,
			ExpiryString: defaultCertDuration.String(),
		},
	}

	return policy
}

// NewSigner returns a CertSigner object after filling in its attibutes
// from the `Config` provided.
func NewSigner(s *SignerCA, policy *config.Signing) (*CertSigner, error) {
	cfs, err := local.NewSigner(s.caKey, s.caCert, signer.DefaultSigAlgo(s.caKey), policy)
	if err != nil {
		return nil, fmt.Errorf("error setting up local cfssl signer: %v", err)
	}

	return &CertSigner{
		caCert:      s.caCert,
		caKey:       s.caKey,
		cfsslSigner: cfs,
	}, nil
}

// Sign sends a signature request to the local signer, receiving
// a signed certificate or an error in response. If successful, It
// then returns the CSR which contains the newly signed certificate.
//
// Note: A signed certificate is issued only for etcd profiles.
func (s *CertSigner) Sign(csr *capi.CertificateSigningRequest) (*capi.CertificateSigningRequest, error) {
	// the following step ensures that the signer server only signs CSRs from etcd nodes
	// that have a specific profile. All other requests are denied immediately.
	profile, err := getProfile(csr)
	if err != nil {
		csr.Status.Conditions = []capi.CertificateSigningRequestCondition{
			capi.CertificateSigningRequestCondition{
				Type:    capi.CertificateDenied,
				Message: fmt.Sprintf("error parsing profile: %v ", err),
			},
		}
		return nil, fmt.Errorf("error parsing profile: %v", err)
	}

	csr.Status.Certificate, err = s.cfsslSigner.Sign(signer.SignRequest{
		Request: string(csr.Spec.Request),
		Profile: profile,
	})
	if err != nil {
		csr.Status.Conditions = []capi.CertificateSigningRequestCondition{
			capi.CertificateSigningRequestCondition{
				Type:    capi.CertificateDenied,
				Message: fmt.Sprintf("certificate signing error: %v ", err),
			},
		}
		return csr, err
	}

	csr.Status.Conditions = []capi.CertificateSigningRequestCondition{
		capi.CertificateSigningRequestCondition{
			Type: capi.CertificateApproved,
		},
	}

	return csr, nil
}

// getProfile returns the profile corresponding to the CSR Subject. For now only
// `etcd-peers` and `etcd-servers` are considered valid profiles.
func getProfile(csr *capi.CertificateSigningRequest) (string, error) {
	x509CSR, err := ParseCSR(csr)
	if err != nil {
		return "", fmt.Errorf("error parsing CSR, %v", err)
	}
	if err := x509CSR.CheckSignature(); err != nil {
		return "", fmt.Errorf("error validating signature of CSR: %v", err)
	}
	if x509CSR.Subject.Organization == nil || len(x509CSR.Subject.Organization) == 0 {
		return "", ErrInvalidOrg
	}

	org := x509CSR.Subject.Organization[0]
	cn := fmt.Sprintf(org[:len(org)-1]+"%s", ":")
	switch org {
	case "system:etcd-peers":
		if strings.HasPrefix(x509CSR.Subject.CommonName, cn) {
			return etcdPeer, nil
		}
		break
	case "system:etcd-servers":
		if strings.HasPrefix(x509CSR.Subject.CommonName, cn) {
			return etcdServer, nil
		}
		break
	case "system:etcd-metrics":
		if strings.HasPrefix(x509CSR.Subject.CommonName, cn) {
			return etcdMetric, nil
		}
		break
	}
	return "", ErrInvalidOrg
}

// HandlePostCSR takes in a CSR, attempts to approve it and writes the CSR
// to a file in the `csrDir`.
// It returns a `http.StatusOK` to the client if the recieved CSR can
// be sucessfully decoded.
func (s *CertServer) HandlePostCSR(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		klog.Errorf("Error reading request body: %v", err)
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}

	obj, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, nil)
	if err != nil {
		klog.Errorf("Error decoding request body: %v", err)
		http.Error(w, "Failed to decode request body", http.StatusInternalServerError)
		return
	}

	csr, ok := obj.(*capi.CertificateSigningRequest)
	if !ok {
		klog.Errorf("Invalid Certificate Signing Request in request from agent: %v", err)
		http.Error(w, "Invalid Certificate Signing Request", http.StatusBadRequest)
		return
	}

	signerCA, err := newSignerCA(s.caFiles, csr)
	if err != nil {
		klog.Errorf("Error signing CSR provided in request from agent: %v", err)
		http.Error(w, "Error signing csr", http.StatusBadRequest)
		return
	}

	signer, err := NewSigner(signerCA, s.policy)
	if err != nil {
		klog.Errorf("Error signing CSR provided in request from agent: %v", err)
		http.Error(w, "Error signing csr", http.StatusBadRequest)
		return
	}

	signedCSR, err := signer.Sign(csr)
	if err != nil {
		klog.Errorf("Error signing CSR provided in request from agent: %v", err)
		http.Error(w, "Error signing csr", http.StatusBadRequest)
		return
	}

	csrBytes, err := json.Marshal(signedCSR)
	if err != nil {
		klog.Errorf("Error marshalling approved CSR: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// write CSR to disk which will then be served to the agent.
	csrFile := path.Join(s.csrDir, signedCSR.ObjectMeta.Name)
	if err := ioutil.WriteFile(csrFile, csrBytes, 0600); err != nil {
		klog.Errorf("Unable to write to %s: %v", csrFile, err)
	}

	// Send the signed CSR back to the client agent
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(csrBytes)

	return
}

// HandleGetCSR retrieves a CSR from a directory location (`csrDir`) and returns it
// to an agent.
func (s *CertServer) HandleGetCSR(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	csrName := vars["csrName"]

	if _, err := os.Stat(filepath.Join(s.csrDir, csrName)); os.IsNotExist(err) {
		// csr file does not exist in `csrDir`
		http.Error(w, "CSR not found with given CSR name"+csrName, http.StatusNotFound)
		return
	}

	data, err := ioutil.ReadFile(filepath.Join(s.csrDir, csrName))
	if err != nil {
		http.Error(w, "error reading CSR from file", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
	return
}

// StartSignerServer initializes a new signer instance.
func StartSignerServer(c Config) error {
	s, err := NewServer(c)
	if err != nil {
		return fmt.Errorf("error setting up signer: %v", err)
	}
	h := &loggingHandler{s.mux}

	certs := make([]tls.Certificate, len(c.ServerCertKeys))
	for idx, pair := range c.ServerCertKeys {
		certs[idx], err = tls.LoadX509KeyPair(pair.CertFile, pair.KeyFile)
		if err != nil {
			return fmt.Errorf("Failed to load key pair from (%q, %q): %v", pair.CertFile, pair.KeyFile, err)
		}
	}
	tlsconfig := &tls.Config{
		Certificates: certs,
	}
	tlsconfig.BuildNameToCertificate()
	return (&http.Server{
		TLSConfig: tlsconfig,
		Handler:   h,
		Addr:      c.ListenAddress,
	}).ListenAndServeTLS("", "")
}

// ParseCSR extracts the CSR from the API object and decodes it.
func ParseCSR(obj *capi.CertificateSigningRequest) (*x509.CertificateRequest, error) {
	// extract PEM from request object
	pemBytes := obj.Spec.Request
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("PEM block type must be CERTIFICATE REQUEST")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	return csr, nil
}
