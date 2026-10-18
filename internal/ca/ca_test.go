// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package ca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestIssueAndVerify(t *testing.T) {
	a, err := New("test")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(a.CertPEM())
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("bad PEM")
	}
	for _, host := range []string{"api.example.com", "127.0.0.1", "::1", "Mixed.Case.Test."} {
		c, err := a.Issue(host)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(c.Certificate[0])
		if err != nil {
			t.Fatal(err)
		}
		name := host
		if host == "Mixed.Case.Test." {
			name = "mixed.case.test"
		}
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: a.Pool(), CurrentTime: time.Now()}); err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if leaf.IsCA {
			t.Fatal("leaf is a CA")
		}
	}
	c1, _ := a.Issue("same.test")
	c2, _ := a.Issue("SAME.test")
	if c1 != c2 {
		t.Fatal("certificate not reused")
	}
	if _, err := a.Issue(""); err == nil {
		t.Fatal("issued for no host")
	}
	other, _ := New("other")
	leaf, _ := x509.ParseCertificate(c1.Certificate[0])
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "same.test", Roots: other.Pool()}); err == nil {
		t.Fatal("another authority accepted the certificate")
	}
	get := a.GetCertificate("fallback.test")
	c3, err := get(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	leaf3, _ := x509.ParseCertificate(c3.Certificate[0])
	if leaf3.DNSNames[0] != "fallback.test" {
		t.Fatal(leaf3.DNSNames)
	}
}
