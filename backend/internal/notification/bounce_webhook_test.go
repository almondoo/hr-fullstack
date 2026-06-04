package notification

import (
	"net/url"
	"strings"
	"testing"
)

// TestSNSCertHostRe locks in the SSRF allowlist hardening for SNS signing-cert
// URLs: only the exact sns.<region>.amazonaws.com host is accepted, so an
// attacker who can host content on another amazonaws.com subdomain (e.g. an S3
// bucket) cannot serve a forged signing certificate.
func TestSNSCertHostRe(t *testing.T) {
	valid := []string{
		"sns.us-east-1.amazonaws.com",
		"sns.ap-northeast-1.amazonaws.com",
		"sns.eu-west-2.amazonaws.com",
	}
	for _, h := range valid {
		if !snsCertHostRe.MatchString(h) {
			t.Errorf("expected %q to be accepted as an SNS signing host", h)
		}
	}

	invalid := []string{
		"evil.s3.amazonaws.com",
		"s3.amazonaws.com",
		"sns.us-east-1.amazonaws.com.evil.com",
		"notsns.us-east-1.amazonaws.com",
		"amazonaws.com",
		"sns..amazonaws.com",
		"attacker.amazonaws.com",
		"sns.us-east-1.amazonaws.com.attacker.net",
	}
	for _, h := range invalid {
		if snsCertHostRe.MatchString(h) {
			t.Errorf("expected %q to be REJECTED as an SNS signing host", h)
		}
	}
}

// TestSNSCertPathValidation mirrors the path allowlist used in
// verifySNSSignature: the path must begin with /SimpleNotificationService- and
// end with .pem.
func TestSNSCertPathValidation(t *testing.T) {
	okPath := func(raw string) bool {
		u, err := url.Parse(raw)
		if err != nil {
			return false
		}
		return strings.HasPrefix(u.Path, "/SimpleNotificationService-") && strings.HasSuffix(u.Path, ".pem")
	}

	valid := []string{
		"https://sns.us-east-1.amazonaws.com/SimpleNotificationService-abc123.pem",
		"https://sns.ap-northeast-1.amazonaws.com/SimpleNotificationService-00000000000000000000000000000000.pem",
	}
	for _, u := range valid {
		if !okPath(u) {
			t.Errorf("expected %q path to be accepted", u)
		}
	}

	invalid := []string{
		"https://sns.us-east-1.amazonaws.com/evil.pem",
		"https://sns.us-east-1.amazonaws.com/SimpleNotificationService-abc.txt",
		"https://sns.us-east-1.amazonaws.com/../SimpleNotificationService-abc.pem",
		"https://sns.us-east-1.amazonaws.com/",
	}
	for _, u := range invalid {
		if okPath(u) {
			t.Errorf("expected %q path to be REJECTED", u)
		}
	}
}
