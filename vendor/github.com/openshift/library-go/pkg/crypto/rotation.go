package crypto

import (
	"crypto/x509"

	"k8s.io/utils/clock"
)

// FilterExpiredCerts checks are all certificates in the bundle valid, i.e. they have not expired.
// The function returns new bundle with only valid certificates or error if no valid certificate is found.
func FilterExpiredCerts(certs ...*x509.Certificate) []*x509.Certificate {
	return FilterExpiredCertsWithClock(clock.RealClock{}, certs...)
}

// FilterExpiredCertsWithClock accepts a list of x509.Certificates and a clock.PassiveClock.
// It returns a list of x509.Certificates whose NotAfter is still after the clock.PassiveClock.Now() value.
func FilterExpiredCertsWithClock(clock clock.PassiveClock, certs ...*x509.Certificate) []*x509.Certificate {
	currentTime := clock.Now()
	var validCerts []*x509.Certificate
	for _, c := range certs {
		if c.NotAfter.After(currentTime) {
			validCerts = append(validCerts, c)
		}
	}

	return validCerts
}
