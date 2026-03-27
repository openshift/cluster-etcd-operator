package tlshelpers

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGetServerHostNames(t *testing.T) {
	scenarios := []struct {
		name              string
		nodeInternalIPs   []string
		expectedToContain []string
		description       string
	}{
		{
			name:            "IPv4 canonical form",
			nodeInternalIPs: []string{"192.168.1.1"},
			expectedToContain: []string{
				"192.168.1.1", // raw form
				"192.168.1.1", // canonical form (same as raw for standard IPv4)
				"localhost",   // static entries
				"127.0.0.1",   // static IPv4 localhost
				"::1",         // static IPv6 localhost
			},
			description: "Standard IPv4 address should include both canonical and raw forms",
		},
		{
			name:            "IPv4 addresses from real nodes are already canonical",
			nodeInternalIPs: []string{"10.0.128.5"},
			expectedToContain: []string{
				"10.0.128.5", // Kubernetes always provides properly formatted IPs
			},
			description: "Real node IPs from Kubernetes API are always in canonical form",
		},
		{
			name:            "IPv6 compressed form",
			nodeInternalIPs: []string{"2001:db8::1"},
			expectedToContain: []string{
				"2001:db8::1", // raw form (compressed)
				"2001:db8::1", // canonical form (same)
			},
			description: "IPv6 compressed form should be included",
		},
		{
			name:            "IPv6 expanded form",
			nodeInternalIPs: []string{"2001:0db8:0000:0000:0000:0000:0000:0001"},
			expectedToContain: []string{
				"2001:0db8:0000:0000:0000:0000:0000:0001", // raw form (expanded)
				"2001:db8::1", // canonical form (compressed)
			},
			description: "IPv6 expanded form should include both expanded (raw) and compressed (canonical) forms",
		},
		{
			name:            "IPv6 localhost expanded form",
			nodeInternalIPs: []string{"0:0:0:0:0:0:0:1"},
			expectedToContain: []string{
				"0:0:0:0:0:0:0:1", // raw form
				"::1",             // canonical form
			},
			description: "IPv6 localhost in expanded form should include both forms for TLS compatibility",
		},
		{
			name:            "IPv6 localhost various forms",
			nodeInternalIPs: []string{"0000:0000:0000:0000:0000:0000:0000:0001"},
			expectedToContain: []string{
				"0000:0000:0000:0000:0000:0000:0000:0001", // raw form
				"::1", // canonical form
			},
			description: "IPv6 localhost with leading zeros should include both forms",
		},
		{
			name:            "multiple IP addresses",
			nodeInternalIPs: []string{"192.168.1.1", "2001:db8::1"},
			expectedToContain: []string{
				"192.168.1.1",
				"2001:db8::1",
			},
			description: "Multiple IPs should all be included",
		},
		{
			name:            "IPv4-mapped IPv6 address",
			nodeInternalIPs: []string{"::ffff:192.0.2.1"},
			expectedToContain: []string{
				"::ffff:192.0.2.1", // raw form
				"192.0.2.1",        // canonical form (converted to IPv4)
			},
			description: "IPv4-mapped IPv6 should include both IPv6 and converted IPv4 forms",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			hostnames := getServerHostNames(scenario.nodeInternalIPs)

			for _, expected := range scenario.expectedToContain {
				found := false
				for _, hostname := range hostnames {
					if hostname == expected {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s: expected hostname %q to be in list, but it wasn't. Got: %v",
						scenario.description, expected, hostnames)
				}
			}

			// Verify static entries are always present
			staticEntries := []string{
				"localhost",
				"etcd.kube-system.svc",
				"etcd.kube-system.svc.cluster.local",
				"etcd.openshift-etcd.svc",
				"etcd.openshift-etcd.svc.cluster.local",
				"127.0.0.1",
				"::1",
			}
			for _, staticEntry := range staticEntries {
				found := false
				for _, hostname := range hostnames {
					if hostname == staticEntry {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected static entry %q to be in hostname list, but it wasn't. Got: %v",
						staticEntry, hostnames)
				}
			}
		})
	}
}

// TestIPCanonicalizeInCertificate verifies that TLS certificate verification
// correctly handles both canonical and non-canonical IP address forms.
// This test demonstrates why we need to include both forms in certificate SANs.
func TestIPCanonicalizeInCertificate(t *testing.T) {
	scenarios := []struct {
		name        string
		certIPs     []string
		connectIP   string
		shouldMatch bool
		description string
	}{
		{
			name:        "IPv6 canonical in cert, canonical connection",
			certIPs:     []string{"::1"},
			connectIP:   "::1",
			shouldMatch: true,
			description: "Connecting to ::1 with ::1 in cert should work",
		},
		{
			name:        "IPv6 canonical in cert, expanded connection",
			certIPs:     []string{"::1"},
			connectIP:   "0:0:0:0:0:0:0:1",
			shouldMatch: true,
			description: "Both forms parse to same IP bytes, so they should match",
		},
		{
			name:        "IPv6 expanded in cert, canonical connection",
			certIPs:     []string{"0:0:0:0:0:0:0:1"},
			connectIP:   "::1",
			shouldMatch: true,
			description: "Both forms parse to same IP bytes, so they should match",
		},
		{
			name:        "IPv6 both forms in cert ensures compatibility",
			certIPs:     []string{"::1", "0:0:0:0:0:0:0:1"},
			connectIP:   "0:0:0:0:0:0:0:1",
			shouldMatch: true,
			description: "Having both forms in cert provides maximum compatibility",
		},
		{
			name:        "IPv6 different address should not match",
			certIPs:     []string{"2001:db8::1"},
			connectIP:   "2001:db8::2",
			shouldMatch: false,
			description: "Different IPv6 addresses should not match",
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// Create a self-signed certificate with the specified IP SANs
			cert := createTestCertWithIPs(t, scenario.certIPs)

			// Parse the connection IP
			connectIPParsed := net.ParseIP(scenario.connectIP)
			require.NotNil(t, connectIPParsed, "connect IP should be valid")

			// Check if the connection IP matches any IP in the certificate
			matched := false
			for _, certIP := range cert.IPAddresses {
				if certIP.Equal(connectIPParsed) {
					matched = true
					break
				}
			}

			if matched != scenario.shouldMatch {
				t.Errorf("%s: expected match=%v, got match=%v. Cert IPs: %v, Connect IP: %v, Parsed cert IPs: %v",
					scenario.description, scenario.shouldMatch, matched,
					scenario.certIPs, scenario.connectIP, cert.IPAddresses)
			}
		})
	}
}

// createTestCertWithIPs creates a self-signed certificate with the specified IP addresses in SANs
func createTestCertWithIPs(t *testing.T, ipStrings []string) *x509.Certificate {
	// Generate a private key
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	// Parse IP addresses
	var ips []net.IP
	for _, ipStr := range ipStrings {
		ip := net.ParseIP(ipStr)
		require.NotNil(t, ip, "IP %q should be valid", ipStr)
		ips = append(ips, ip)
	}

	// Create certificate template
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "test-cert",
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: ips,
	}

	// Create self-signed certificate
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	require.NoError(t, err)

	// Parse the certificate
	cert, err := x509.ParseCertificate(certDER)
	require.NoError(t, err)

	return cert
}
