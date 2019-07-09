package certsigner

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"testing"
	"time"

	capi "k8s.io/api/certificates/v1beta1"
)

var (
	caCrtFile        = "ca.crt"
	caKeyFile        = "ca.key"
	caMetricsCrtFile = "metric-ca.crt"
	caMetricsKeyFile = "metric-ca.key"

	csrBytes = []byte(`-----BEGIN CERTIFICATE REQUEST-----
MIICojCCAYoCAQAwXTELMAkGA1UEBhMCWFgxFTATBgNVBAcMDERlZmF1bHQgQ2l0
eTEaMBgGA1UECgwRc3lzdGVtOmV0Y2QtcGVlcnMxGzAZBgNVBAMMEnN5c3RlbTpl
dGNkLXBlZXI6MTCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBANALpBiW
8vVBDwgRGuj0FqOk85trY9YHJUsGlRCthnvKfWt7WGTwEWpui8yk1N0i1YY0aHtA
Q450aeg+cBx7Ptmubyt+vvS1swTuKe7cRKj7Nws8ukMT2EK6Ju3es5zeiomWU67k
q4U0D2XQenVVP5flr/c/KW58AfRL0+XOzQxrLao5Sm9Yp2o3/bcpZv7LN95tigO3
/yXxqoGp/9q9UCvRxaaPHjKryh6ZcuG9Acar5HGen8pcr1jiNwvRt6CtKd8hFgGh
uWU481VGuXvCwKgNv8R8RelvdCJtxpqaZu8t6TpY2im64WJx/Q+oVJIKYqQ1I1Zy
aAgiQUhSAyTjFvECAwEAAaAAMA0GCSqGSIb3DQEBCwUAA4IBAQCv9pasM29bp06+
48oc8UMwjVz5DqlIGi4NyJ6DqUpKJr+C68sMnxqKqinGNTI3XedPHv5weKRckFRk
MPVwDE1P3IxkmghoVtUJ9hM0AwRXaGD8pHWXq3JiryxoIGXCz2p0oEGczKmkxBki
0sBVniocg2njSzIJeBag9kZwskhwfralaEkOpxs7iNBHmBDg3IdSCZqO+ikVg0b/
X6IGYVFVglScoQS4xQGiyhxzZhgAjKFsRaAWjcpU6LkSpF9org3KpZtcKeV/ZwZT
5KuRy6rsTWvlX/8onttDqtsipBkyVKlBsrsnfO3A0XwhEt79h9fnxMK94K0quTVA
jvINeymP
-----END CERTIFICATE REQUEST-----`)

	csrMetricsBytes = []byte(`-----BEGIN CERTIFICATE REQUEST-----
MIIEpjCCAo4CAQAwYTELMAkGA1UEBhMCWFgxFTATBgNVBAcMDERlZmF1bHQgQ2l0
eTEcMBoGA1UECgwTc3lzdGVtOmV0Y2QtbWV0cmljczEdMBsGA1UEAwwUc3lzdGVt
OmV0Y2QtbWV0cmljOjEwggIiMA0GCSqGSIb3DQEBAQUAA4ICDwAwggIKAoICAQDJ
kSE7th+Tu26k1me4guVF96CD7xA1xs9ujR7+/KMGKPYq+oPFZKOMX5xJziY4uF29
Nhp3VTTKVgU8QOT6Y0yaG4yjp6BPCSXY0/HEVPe2Z87xdPDGU2vrB5oeHVFRXDZ1
vDpODQ0M+F97aLqpQKXtthBjGaEmJH/DB4BqSGOH+5fDTggCWa0NfiscLNX99Tkf
jb1+fC/7bDpz3i/0op34pvS+hv1U9vFLlZpz7imka42lh6xutnOvUItTsngzZ2c+
54oUmb1Vyji1NG7rGZWsHtjPYXgvHq85hQwqlbVFYt169pfcDQGYwy+NSh847AN8
ORa2OQt4V9B8ZTTDdn4CQ93nTMTwKKf2dwcTxj84ABl4eYFWPIw6xOsPEOCtgNsw
2GpD5csh7hdsjI0WOw81fUfA9xbtc7SvP5vinCzIt2RE3sFuKXQPmlVLrtG5nQQg
aH/HXFXxYKrJhFTiDyGay1pat1UhDbmio8CCcSgSgb8O/M9ZSVVh0fEHOr2e38vo
K1p0VgHZ3ITL4/a/3Ex70sA/PWzYvrGb/FOhP1CRLx0CstQjVgp4dQqPlZJWy3qy
iyZgR5xCgx9nElRKh2ntJmyv9msMmYQUgNmt2uPHoOAaYAj49lz5VtttwXv23YwX
20ODXe3d6pwDeCGbBj2Atkp2Jvc5z4aG/ZqyeQUVjwIDAQABoAAwDQYJKoZIhvcN
AQELBQADggIBAAlddAVwfZeZqJQwd/0WsLq0mH3TcevKkRAy/mUP/Jna+ug8I+xw
DjWjKXv0EyXif3IFP7mzrmaW1ksqcOwoUBGMMc17kcXVRP9VGWM0YmXYU2TrjIkg
YQTVdQXP5+W7VwHL6m/rdsOt/zy1OFVAn88f5hrNEnkmv7MwwK+BgWMdRT3lTWbi
HQF86vjW/0joR+L0vGBJxlGJma+c6wxxsPPi0eRDJuThV9Yzr0VhXLiLlimm1e2N
dT9JL2n/RAtb6LbJfqqFWvpr64ZnPz/s+bhMnd+Ufk5cVMqov4jvZijZQlhqR8R/
qSaopgZfFdQuL2uZLTHrDNTZfLIXiu7qIyRvlpCPJgyJ29LVBzxPurg1euhln/pr
G94cfrZe/dMEQvEQrSSyUcxOXzeYm3+uT+xIp7yK5z5V8w7qmR+PUqRIKOZ6TBSU
YxCa7IieBKN82wm9X2FulifD1LVUvHaztGnhipV2UmLo0Wrwo78jHJz0nw0bYEMd
Ba+1yisQjcSz21Zsl7lbm2MKfrR8cx5AoP2Xn75jGe9L+n6YEx2MEHZp7/s7X1hf
Ax2vGHbg+P6Yof8alxqe2AHzAtlbUSmwXsf8efs/3Uey07x55fC7O42hj5VNLbfV
qqOj7/EZleCtQplqyOwF8Mt6h4LZBE4lgB27HNtX5VAcZsUnVTtJrO0e
-----END CERTIFICATE REQUEST-----`)

	caCrtBytes = []byte(`-----BEGIN CERTIFICATE-----
MIIFDTCCAvWgAwIBAgIJAMPkKOVfXGKZMA0GCSqGSIb3DQEBCwUAMBIxEDAOBgNV
BAMMB2Zha2UtY2EwHhcNMTgwMjIyMTg1MTA3WhcNMjgwMjIwMTg1MTA3WjASMRAw
DgYDVQQDDAdmYWtlLWNhMIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEA
pOcWmKHVuJIy1mTWG4/PfDj1zL7YHpg2SQ4uIL2mUyGVT5sqRkz1fPsDQKzm4iYs
pMad7rd/0hseNTENt+ZbmLUovnFeqQ/U5OGXnpzarLZexQEcbpw5xHoahQAFKrAC
arBOMXhShF828OqdbSiL9w9BTMs9dcLzu53AsA3dC6qDsW7mud2Bz9ais/GyIL7P
7ZWXZwCisixbAc88CAivECyfSvhiELFeh2wC2k/fH7aG//gH95gSyrV7Ai1n/1tF
TE+SW7LvLSJVUBfUPIlS2UGsO+/yQGnQHmA2WvssTsNIoMOqJ5xM3T0ZRSPC39hL
9F5ubCMmcfCj55DeAFGJa0/s3YSO0Y/XFWqqf5pAGNhkTTK6ZYCpqNOcDaRqkYUt
YTuMBRHD7mW1kmxko6L2nlfp2cvRiMEFCrATZB2haHWPj2ly0E+oRpCp1Pr+Zou6
37YFhpCmxI1PnfypWrFbjzG4U3AU+9+qjLvDwbA8YO5O+AybbtA3lvdUHWPnJWvV
v9Xtl7UXg6PjGmOHEDF+lVBvvfTbaXLqYmt8RGJbaxvZD9RYnLBswl/13tf93FuC
E+XJNabuS9PA42Cu3xx7FM582uM60uiEbk7DEsJs93NWrlAO1Sefmcw0KL3oHvCU
hK4x04OiqOpx6EOkJ58yfc45U1sPnWos5wy7HKTAxTkCAwEAAaNmMGQwHQYDVR0O
BBYEFE24MCpdZZHhv4hRu49eNKdjI/INMB8GA1UdIwQYMBaAFE24MCpdZZHhv4hR
u49eNKdjI/INMBIGA1UdEwEB/wQIMAYBAf8CAQAwDgYDVR0PAQH/BAQDAgGGMA0G
CSqGSIb3DQEBCwUAA4ICAQAoWfLQ6gKZFLRz4pyWcbP6ukBafwmwhdA2YCdlT/ay
jUetwFhgiFE3xiymfiGwD2TsB+vmg7NIu0zMhCotnpBclIz6TU3ll4IuryuJYkgW
Nfj9f1P6q1feJl2QGutfWFO566FVGikD09X/99Wm+vqIusrJA10hbUFEB/G188pz
AKjeKt0VjRCKQwvNLAWcqyQRXAq7mqwkCDSOdL0sFyNaSGVJsfde7wtuQ4NKVKhi
5v7fpTvYkJrtr6KnTjG5CgwwBSC6Hu1+kA97NMlGEidkprjNrA6L8Qiloy1cOWM6
oD7Fan+KgbeMcFSB6H1v0xiqUQShWhLgL22+HngAoneM+t+tq9QfAsw4l3GZrdjX
XQ9SWwCr66rrOBJiQl+e1/t7B2r8dO7Zypuy5uR28nXX7BNiDP58wWQqeux6aLz2
qlPD15R+FkgmNXqjPAhwou7Eaigy1KbKhhdqj2EMkQZhe+RWNDP6gKXFLmj5vbEL
E/Mb0p/otozmx7Y7QIkXSh//H/x8Bi/vLRtNQmbgCYu6iNNOKRTiVtaYRcy7vj4Z
iE6rkr//NhxuZeaBDItIRC4uRcSF8noeFkGuQGb22vf8HnwDKnNF9Ty6Zg8CfRVv
Rt0zd4OjeRzVNivCQ3ilpj5uv2vob9+9svKVatdFYst93eaBvWGd1hbsev7T/3t3
bA==
-----END CERTIFICATE-----`)

	caMetricsCrtBytes = []byte(`-----BEGIN CERTIFICATE-----
MIIDoDCCAoigAwIBAgIJAKnsPQEyA7F4MA0GCSqGSIb3DQEBCwUAMGUxCzAJBgNV
BAYTAlVTMQswCQYDVQQIDAJDQTEVMBMGA1UEBwwMRGVmYXVsdCBDaXR5MRgwFgYD
VQQKDA9mYWtlLW1ldHJpY3MtY2ExGDAWBgNVBAsMD2Zha2UtbWV0cmljcy1jYTAe
Fw0xOTA0MjQyMjQ1MTNaFw0yOTA0MjEyMjQ1MTNaMGUxCzAJBgNVBAYTAlVTMQsw
CQYDVQQIDAJDQTEVMBMGA1UEBwwMRGVmYXVsdCBDaXR5MRgwFgYDVQQKDA9mYWtl
LW1ldHJpY3MtY2ExGDAWBgNVBAsMD2Zha2UtbWV0cmljcy1jYTCCASIwDQYJKoZI
hvcNAQEBBQADggEPADCCAQoCggEBAM9sN8qC5hvICzYXZ3xfD7nSG+w2dqv7cH7P
7zRDf9JNQpV9Pl/YwDPRUWF4RO4NGbPlH1sRsLYlX6HiRVx1v7iEY4g3mSvkCeUb
Matjj35iwwb3BHA+gTWcCLB8GU5oNVuycVOB2D7LG76UzTSp1aEkEbXFrJUmI+dR
TobYZB+ECvOrVCRcm/iz2rhiF/fEqJawxOU5A7qiJuV87NXRXNY48DmKTHBZHC//
cYzV+m99yQ0CWpz+Y7bxPKceLeUMUngvJlE/dgyHBLUebK1ZymzWTyU+to+nCfS9
B7CSSNUGauf5+LGrhzzGdxQ3n6fZG+f8uoo9ILKgfLyuwIOXpO0CAwEAAaNTMFEw
HQYDVR0OBBYEFBgrZJppU/8uMe2hLNqnNa/6j4pCMB8GA1UdIwQYMBaAFBgrZJpp
U/8uMe2hLNqnNa/6j4pCMA8GA1UdEwEB/wQFMAMBAf8wDQYJKoZIhvcNAQELBQAD
ggEBABMHy+0oA08iyP355WXW7QQR7sxASPzNOKyiHeZsg7cvAwWxUpK71Z3fcvR8
bcBqmT2wnCm1yOj2Mw5hbRnntTgKZAzbIoRtvAhI04i5e9SUjbI9RVQz5G11SKAm
tg2zZPvEu6OpCBc/PHE4BdazprvFJDSeQTG1Y+5/zJ04XOXCqhLndKZfYCnRAbgu
b367F/raoB/iVDZPmJQiXRuvkXpPl/a+2rwiltUzNbeHDPLCRfMsoWNBE5/Xrajs
lXXy+a/6GRLGk7Jt+HC1qDTskUNtexQsvh8yjw5gH3SmtWDP3TeV7HxaXhIr5GVo
0ARjPTCTS2mm7tNyRJLkFJonnjo=
-----END CERTIFICATE-----
`)

	caKeyBytes = []byte(`-----BEGIN RSA PRIVATE KEY-----
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
-----END RSA PRIVATE KEY-----`)

	caMetricsKeyBytes = []byte(`-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEAz2w3yoLmG8gLNhdnfF8PudIb7DZ2q/twfs/vNEN/0k1ClX0+
X9jAM9FRYXhE7g0Zs+UfWxGwtiVfoeJFXHW/uIRjiDeZK+QJ5Rsxq2OPfmLDBvcE
cD6BNZwIsHwZTmg1W7JxU4HYPssbvpTNNKnVoSQRtcWslSYj51FOhthkH4QK86tU
JFyb+LPauGIX98SolrDE5TkDuqIm5Xzs1dFc1jjwOYpMcFkcL/9xjNX6b33JDQJa
nP5jtvE8px4t5QxSeC8mUT92DIcEtR5srVnKbNZPJT62j6cJ9L0HsJJI1QZq5/n4
sauHPMZ3FDefp9kb5/y6ij0gsqB8vK7Ag5ek7QIDAQABAoIBAHU3xt+e0cNpbUyI
NWdHoW91mWoH7VCLq6s+fwOeEaIbH0GzoYgwyY1/AOqAORP+O0Q6e1nPyXll7YFi
iagSsuHnjwfvw5PWLvFWSN9+SB04WtaYyd1UtVhCcXaq6vIwWdcUJI74legGiAtP
tBfK0ntaEtgSedFf2HJktGfn6c0UoDdSMTpkua61bonAR4Z7JwyRj4SGHkEBOkhL
cHNRnTgVfCQ4jngqyGljMK0E+qkLdlqpGIL0qLC3Va1GXxXBLDwOUK9yBHCSMIpc
CDDVll4TgYUBX+MPfFTgu7U0/+9CfcIadQj6QQ9El1/0CVVBTgucO4ikCeFBxevr
h14ID4ECgYEA/O+mprUjesJQbAqRyjOYkaNifHjK/MJySMtCC2GfA/6cZDzH85gd
Igb8TZTL7hU6e7kk2G/9qBHmlK3EqxgPTHEnB9utUYKgD8cnqWLoDTwo/bLaBcsl
ebkHFn+1xE2g8hN2CGVQ3sfVqNGblHFBQo4cKchK2Vg2GH+0/6UmTKkCgYEA0e9u
UdR8s+u3FsLyDdnUkhmYWna7QjoT3UMxjtH+o96IXJXmoZx6BFyGUdY2nstN1yRe
VHHB3LP/20ky8DvUnWx1Gvad3dm70SXy39iTCJ5KnWT9xo1EqEkAnhCzc4z+B+iP
K4adcYIXDaeykZPOfOMjO4m9SUeW1C0mhY0v3KUCgYEA1+Gz25W/MoenHI/o3ywq
jCNna9Wtaw6LfJX/SLeJgV9PHD7EaqTqOKC9t3nIlOyJfhAH4rOzTD/7DetCcMWY
SSZKqepVg7x54P2aXHiOlr1CP0bnzwoUck/6PLnD6khXlkYF+CSBYaQuOGiu4YPI
r4WbhA3v1JH1mfNmCMxsZAECgYBfSE2I5GlI+/YYZZiZAsIBIY7NmE/7igKUDThD
+zmYxJqdcwe/WBblPd1U7WXTArEssXwC1bLIagX5UCrHcFBatuwbtc0G8RjWn2Ox
h0mMwtNYxoqMAHgl7SRTmX7pNhfiHQJGHg39g67U6sUYX757XlgSYLzBsrVZTbjL
Kr6LZQKBgQDGq4+tJYhT0+zYJ3Ds97SzAfFIXIPzDWhpRbbwvv98SQrfBppiiYqd
sDKkMqkhqffxfUae8TtI1dLEfs/+LA4YhcafoEb9zQ6BZ8g2n0zhaJ0S7NNHoMpj
AXT0pY+qS78LFp0sm+YSG4gIAO5MFvXhVo2uXgT/oBI/e2PzfLV+sA==
-----END RSA PRIVATE KEY-----
`)
)

func loadAllCrts(t *testing.T) {
	loadCrt(caKeyBytes, caKeyFile, t)
	loadCrt(caCrtBytes, caCrtFile, t)
	loadCrt(caMetricsKeyBytes, caMetricsKeyFile, t)
	loadCrt(caMetricsCrtBytes, caMetricsCrtFile, t)
}

func loadCrt(data []byte, filename string, t *testing.T) {
	if err := ioutil.WriteFile(filename, data, 0644); err != nil {
		t.Fatalf("error writing credentials to file: %v", err)
	}
}

func TestNewSignerCA(t *testing.T) {
	type TestData struct {
		SignerCAFiles
		csrFileBytes []byte
		want         string
	}

	for _, test := range []TestData{
		{
			SignerCAFiles{caCrtFile, caKeyFile, "", ""},
			csrBytes,
			"ok",
		},
		{
			SignerCAFiles{caCrtFile, caKeyFile, "", ""},
			csrMetricsBytes,
			"error",
		},
		{
			SignerCAFiles{caCrtFile, caKeyFile, caMetricsCrtFile, caMetricsKeyFile},
			csrMetricsBytes,
			"ok",
		},
		{
			SignerCAFiles{caCrtFile, caKeyFile, caMetricsCrtFile, caMetricsKeyFile},
			csrBytes,
			"ok",
		},
		{
			SignerCAFiles{"", "", caMetricsCrtFile, caMetricsKeyFile},
			csrBytes,
			"error",
		},
		{
			SignerCAFiles{"", "", caMetricsCrtFile, caMetricsKeyFile},
			csrMetricsBytes,
			"ok",
		},
	} {

		loadAllCrts(t)
		caFiles := test.SignerCAFiles
		csr := createCSR(test.csrFileBytes)
		profile, _ := getProfile(csr)
		_, err := newSignerCA(&caFiles, csr)
		got := gotError(err)
		if test.want != got {
			t.Errorf("NewSignerCA profile %s want (%s) got (%s) error: %v", profile, test.want, got, err)
		}
		if err := cleanUp(caFiles); err != nil {
			t.Fatalf("NewSignerCA error deleting files %v", err)
		}
	}
}

func TestSign(t *testing.T) {
	type TestData struct {
		SignerCAFiles
		csrFileBytes []byte
		caCrt        []byte
		dnsName      string
		want         string
	}

	for _, test := range []TestData{
		{
			SignerCAFiles{caCrtFile, caKeyFile, "", ""},
			csrBytes,
			caCrtBytes,
			"system:etcd-peer:1",
			"ok",
		},
		{
			SignerCAFiles{caCrtFile, caKeyFile, caMetricsCrtFile, caMetricsKeyFile},
			csrMetricsBytes,
			caMetricsCrtBytes,
			"system:etcd-metric:1",
			"ok",
		},
		{
			SignerCAFiles{caCrtFile, caKeyFile, caMetricsCrtFile, caMetricsKeyFile},
			csrMetricsBytes,
			caMetricsCrtBytes,
			"google.com",
			"error",
		},
	} {

		loadAllCrts(t)

		caFiles := test.SignerCAFiles
		config := Config{
			SignerCAFiles:          caFiles,
			ListenAddress:          "0.0.0.0:6443",
			EtcdMetricCertDuration: 1 * time.Hour,
			EtcdPeerCertDuration:   1 * time.Hour,
			EtcdServerCertDuration: 1 * time.Hour,
		}

		csr := createCSR(test.csrFileBytes)
		policy := signerPolicy(config)
		signerCA, err := newSignerCA(&caFiles, csr)
		if err != nil {
			t.Errorf("error setting up signerCA:%v", err)
		}
		s, err := NewSigner(signerCA, &policy)
		if err != nil {
			t.Errorf("error setting up signer:%v", err)
		}

		signedCSR, errMsg := s.Sign(csr)
		if errMsg != nil {
			t.Errorf("error signing csr: %v", errMsg)
		}
		if len(signedCSR.Status.Certificate) == 0 {
			t.Errorf("csr not signed: %v", err)
		}

		roots := x509.NewCertPool()
		ok := roots.AppendCertsFromPEM(test.caCrt)
		if !ok {
			t.Errorf("failed to parse root certificate")
		}
		block, _ := pem.Decode([]byte(signedCSR.Status.Certificate))
		if block == nil {
			t.Errorf("failed to parse certificate PEM")
		}

		opts := x509.VerifyOptions{
			DNSName: test.dnsName,
			Roots:   roots,
		}
		cert, _ := x509.ParseCertificate(block.Bytes)
		_, verr := cert.Verify(opts)
		got := gotError(verr)
		if got != test.want {
			t.Errorf("TestSign want %s got %s err %v", test.want, got, verr)
		}
		// cleanup
		if err := cleanUp(caFiles); err != nil {
			t.Fatalf("error deleting files %v", err)
		}
	}
}

func createCSR(csr []byte) *capi.CertificateSigningRequest {
	return &capi.CertificateSigningRequest{
		Spec: capi.CertificateSigningRequestSpec{
			Request: csr,
			Usages: []capi.KeyUsage{
				capi.UsageSigning,
				capi.UsageKeyEncipherment,
				capi.UsageServerAuth,
				capi.UsageClientAuth,
			},
		},
	}
}

func cleanUp(files SignerCAFiles) error {
	f := reflect.ValueOf(files)
	for i := 0; i < f.NumField(); i++ {
		file := fmt.Sprintf("%s", f.Field(i).Interface())
		if file == "" {
			continue
		}
		if err := os.Remove(file); err != nil {
			return err
		}
	}
	return nil
}

func gotError(err error) string {
	switch t := err.(type) {
	case nil:
		return "ok"
	case error:
		return "error"
	default:
		return fmt.Sprintf("invalid type: %v", t)
	}
}
