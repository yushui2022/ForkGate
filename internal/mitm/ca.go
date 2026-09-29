package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxCachedLeaves = 256

type Authority struct {
	cert       *x509.Certificate
	key        *rsa.PrivateKey
	certPEM    []byte
	cacheMu    sync.Mutex
	leafByHost map[string]tls.Certificate
	lastUsed   map[string]uint64
	cacheClock uint64
}

func LoadOrCreate(dir string) (*Authority, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create CA directory: %w", err)
	}
	certPath := filepath.Join(dir, "ca-cert.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		cert, key, err := parseCA(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		return newAuthority(cert, key, certPEM), nil
	}
	// A partial pair may be the only remaining copy of an existing identity.
	// Never replace it, or treat unreadable files as permission to rotate it.
	if !errors.Is(certErr, os.ErrNotExist) || !errors.Is(keyErr, os.ErrNotExist) {
		if certErr != nil {
			return nil, fmt.Errorf("read existing CA certificate: %w", certErr)
		}
		return nil, fmt.Errorf("read existing CA key: %w", keyErr)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ForkGate Local CA", Organization: []string{"ForkGate"}},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER := x509.MarshalPKCS1PrivateKey(key)
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})
	// Exclusive creation also prevents a simultaneous first start from
	// overwriting the other process's CA files. A failed pair stays fail-closed.
	if err := writeExclusive(keyPath, keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write CA key: %w", err)
	}
	if err := writeExclusive(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse generated CA: %w", err)
	}
	return newAuthority(cert, key, certPEM), nil
}

func newAuthority(cert *x509.Certificate, key *rsa.PrivateKey, certPEM []byte) *Authority {
	return &Authority{cert: cert, key: key, certPEM: certPEM,
		leafByHost: make(map[string]tls.Certificate), lastUsed: make(map[string]uint64)}
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	return errors.Join(writeErr, closeErr)
}

func (a *Authority) CAPEM() []byte {
	return append([]byte(nil), a.certPEM...)
}

func (a *Authority) Leaf(host string) (tls.Certificate, error) {
	host = normalizeHost(host)
	if host == "" || len(host) > 253 {
		return tls.Certificate{}, fmt.Errorf("invalid certificate host")
	}
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	now := time.Now()
	if now.Before(a.cert.NotBefore) || !now.Before(a.cert.NotAfter) {
		return tls.Certificate{}, fmt.Errorf("CA certificate is not currently valid")
	}
	a.cacheClock++
	if leaf, ok := a.leafByHost[host]; ok {
		if leaf.Leaf != nil && !now.Before(leaf.Leaf.NotBefore) && now.Before(leaf.Leaf.NotAfter) {
			a.lastUsed[host] = a.cacheClock
			return leaf, nil
		}
		delete(a.leafByHost, host)
		delete(a.lastUsed, host)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	notBefore, notAfter := now.Add(-time.Minute), now.Add(24*time.Hour)
	if notBefore.Before(a.cert.NotBefore) {
		notBefore = a.cert.NotBefore
	}
	if notAfter.After(a.cert.NotAfter) {
		notAfter = a.cert.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"ForkGate MITM"}},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create leaf certificate: %w", err)
	}
	parsedLeaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse leaf certificate: %w", err)
	}
	leaf := tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key, Leaf: parsedLeaf}
	if len(a.leafByHost) >= maxCachedLeaves {
		var oldestHost string
		oldestUse := ^uint64(0)
		for cachedHost, used := range a.lastUsed {
			if used < oldestUse {
				oldestHost, oldestUse = cachedHost, used
			}
		}
		delete(a.leafByHost, oldestHost)
		delete(a.lastUsed, oldestHost)
	}
	a.leafByHost[host] = leaf
	a.lastUsed[host] = a.cacheClock
	return leaf, nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *rsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("invalid CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "RSA PRIVATE KEY" {
		return nil, nil, fmt.Errorf("invalid CA key PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	if err := key.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid CA private key: %w", err)
	}
	if !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, fmt.Errorf("certificate is not a signing CA")
	}
	now := time.Now()
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return nil, nil, fmt.Errorf("CA certificate is not currently valid")
	}
	publicKey, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || publicKey.E != key.E || publicKey.N.Cmp(key.N) != 0 {
		return nil, nil, fmt.Errorf("CA certificate and private key do not match")
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return nil, nil, fmt.Errorf("CA certificate is not self-signed: %w", err)
	}
	if cert.Issuer.String() != cert.Subject.String() {
		return nil, nil, fmt.Errorf("CA certificate issuer does not match subject")
	}
	return cert, key, nil
}

func normalizeHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.TrimSuffix(host, ".")
}

func randomSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 120)
	serial, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return serial, nil
}
