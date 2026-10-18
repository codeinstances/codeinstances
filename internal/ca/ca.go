// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package ca is a small certificate authority that lives for one run.
//
// To see a program's HTTPS traffic, codeinstances stands in for the servers
// it talks to. Each run makes a fresh authority, hands its certificate to the
// program to trust, and signs a certificate for each host the program asks
// for. The key never leaves memory and dies with the run.
package ca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

// Authority signs certificates for any host.
type Authority struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certPEM []byte

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// New makes an authority valid from a day ago for a week, which covers clock
// skew and any run's length.
func New(name string) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name, Organization: []string{"CodeInstances"}},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.Add(7 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Authority{
		cert:    cert,
		key:     key,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		cache:   map[string]*tls.Certificate{},
	}, nil
}

// CertPEM is the authority's certificate, for the program to trust.
func (a *Authority) CertPEM() []byte { return a.certPEM }

// Pool is a certificate pool holding only this authority.
func (a *Authority) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	return pool
}

// Issue returns a certificate for host (a name or an IP address), signed by
// the authority. Certificates are made once per host and reused.
func (a *Authority) Issue(host string) (*tls.Certificate, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return nil, errors.New("no host name")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.cache[host]; ok {
		return c, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    a.cert.NotBefore,
		NotAfter:     a.cert.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, err
	}
	c := &tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key}
	a.cache[host] = c
	return c, nil
}

// GetCertificate issues for the name the client asked for, or for fallback
// when the client sent none (a client connecting by IP address).
func (a *Authority) GetCertificate(fallback string) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		name := hello.ServerName
		if name == "" {
			name = fallback
		}
		return a.Issue(name)
	}
}

func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}
