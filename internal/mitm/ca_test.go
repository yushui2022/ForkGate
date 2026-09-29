package mitm

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOrCreatePreservesIdentity(t *testing.T) {
	dir := t.TempDir()
	a := mustAuthority(t, dir)
	beforeCert := readFile(t, filepath.Join(dir, "ca-cert.pem"))
	beforeKey := readFile(t, filepath.Join(dir, "ca-key.pem"))
	reloaded := mustAuthority(t, dir)
	if !bytes.Equal(a.CAPEM(), reloaded.CAPEM()) || a.key.N.Cmp(reloaded.key.N) != 0 {
		t.Fatal("reload changed CA identity")
	}
	if !bytes.Equal(beforeCert, readFile(t, filepath.Join(dir, "ca-cert.pem"))) ||
		!bytes.Equal(beforeKey, readFile(t, filepath.Join(dir, "ca-key.pem"))) {
		t.Fatal("reload changed CA files")
	}
	returned := reloaded.CAPEM()
	returned[0] ^= 1
	if bytes.Equal(returned, reloaded.CAPEM()) {
		t.Fatal("CAPEM exposes its internal buffer")
	}
}

func TestPartialCAIsNeverReplaced(t *testing.T) {
	for _, missing := range []string{"ca-key.pem", "ca-cert.pem"} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			mustAuthority(t, dir)
			remaining := "ca-cert.pem"
			if missing == remaining {
				remaining = "ca-key.pem"
			}
			before := readFile(t, filepath.Join(dir, remaining))
			if err := os.Remove(filepath.Join(dir, missing)); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreate(dir); err == nil {
				t.Fatal("partial CA should fail")
			}
			if !bytes.Equal(before, readFile(t, filepath.Join(dir, remaining))) {
				t.Fatal("partial CA was overwritten")
			}
			if _, err := os.Stat(filepath.Join(dir, missing)); !os.IsNotExist(err) {
				t.Fatalf("missing CA file was recreated: %v", err)
			}
		})
	}
}

func TestUnreadableCAIsNeverReplaced(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca-cert.pem")
	if err := os.Mkdir(certPath, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "ca-key.pem")
	before := []byte("existing private key")
	writeFile(t, keyPath, before)
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("unreadable CA should fail")
	}
	if !bytes.Equal(before, readFile(t, keyPath)) {
		t.Fatal("read error overwrote existing key")
	}
	info, err := os.Stat(certPath)
	if err != nil || !info.IsDir() {
		t.Fatalf("read error replaced certificate path: %v", err)
	}
}

func TestInvalidCADoesNotOverwriteFiles(t *testing.T) {
	valid := mustAuthority(t, t.TempDir())
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := encodeKey(valid.key)
	cases := []struct {
		name string
		cert []byte
		key  []byte
	}{
		{"bad certificate", []byte("invalid PEM"), keyPEM},
		{"bad key", valid.CAPEM(), []byte("invalid PEM")},
		{"mismatch", valid.CAPEM(), encodeKey(otherKey)},
	}
	for _, tc := range []struct {
		name   string
		modify func(*x509.Certificate)
	}{
		{"not CA", func(c *x509.Certificate) { c.IsCA = false; c.MaxPathLen = -1 }},
		{"no signing usage", func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageDigitalSignature }},
		{"expired", func(c *x509.Certificate) { c.NotAfter = time.Now().Add(-time.Hour) }},
		{"not yet valid", func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Hour) }},
	} {
		cert := *valid.cert
		tc.modify(&cert)
		der, err := x509.CreateCertificate(rand.Reader, &cert, &cert, &valid.key.PublicKey, valid.key)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, struct {
			name string
			cert []byte
			key  []byte
		}{tc.name, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			certPath, keyPath := filepath.Join(dir, "ca-cert.pem"), filepath.Join(dir, "ca-key.pem")
			writeFile(t, certPath, tc.cert)
			writeFile(t, keyPath, tc.key)
			if _, err := LoadOrCreate(dir); err == nil {
				t.Fatal("invalid CA should fail")
			}
			if !bytes.Equal(tc.cert, readFile(t, certPath)) || !bytes.Equal(tc.key, readFile(t, keyPath)) {
				t.Fatal("invalid CA files were overwritten")
			}
		})
	}
}

func TestLeafCertificatesVerifyForDNSAndIP(t *testing.T) {
	a := mustAuthority(t, t.TempDir())
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	for _, tc := range []struct{ input, name string }{
		{"Example.TEST:443", "example.test"},
		{"127.0.0.1:443", "127.0.0.1"},
		{"[::1]:443", "::1"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			leaf, err := a.Leaf(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			cert, err := x509.ParseCertificate(leaf.Certificate[0])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: tc.name}); err != nil {
				t.Fatalf("leaf verification failed: %v", err)
			}
			cached, err := a.Leaf(tc.name)
			if err != nil || !bytes.Equal(leaf.Certificate[0], cached.Certificate[0]) {
				t.Fatalf("normalized host did not reuse leaf: %v", err)
			}
		})
	}
}

func TestExpiredLeafIsRefreshed(t *testing.T) {
	a := mustAuthority(t, t.TempDir())
	first, err := a.Leaf("example.test")
	if err != nil {
		t.Fatal(err)
	}
	a.leafByHost["example.test"].Leaf.NotAfter = time.Now().Add(-time.Minute)
	refreshed, err := a.Leaf("example.test")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Certificate[0], refreshed.Certificate[0]) || !refreshed.Leaf.NotAfter.After(time.Now()) {
		t.Fatal("expired leaf was reused")
	}
	if len(a.leafByHost) != 1 || len(a.lastUsed) != 1 {
		t.Fatal("refresh grew the cache")
	}
}

func TestLeafCacheIsBoundedAndEvictsLeastRecentlyUsed(t *testing.T) {
	a := mustAuthority(t, t.TempDir())
	for i := 0; i < maxCachedLeaves; i++ {
		if _, err := a.Leaf(fmt.Sprintf("host-%d.test", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Leaf("host-0.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Leaf("new.test"); err != nil {
		t.Fatal(err)
	}
	if len(a.leafByHost) != maxCachedLeaves || len(a.lastUsed) != maxCachedLeaves {
		t.Fatalf("cache exceeds limit: %d entries", len(a.leafByHost))
	}
	if _, ok := a.leafByHost["host-1.test"]; ok {
		t.Fatal("least recently used leaf was retained")
	}
	if _, ok := a.leafByHost["host-0.test"]; !ok {
		t.Fatal("recently used leaf was evicted")
	}
}

func TestLeafCannotOutliveCA(t *testing.T) {
	a := mustAuthority(t, t.TempDir())
	a.cert.NotAfter = time.Now().Add(time.Hour).Truncate(time.Second)
	leaf, err := a.Leaf("example.test")
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Leaf.NotAfter.After(a.cert.NotAfter) {
		t.Fatal("leaf outlives its CA")
	}
	a.cert.NotAfter = time.Now().Add(-time.Minute)
	if _, err := a.Leaf("example.test"); err == nil {
		t.Fatal("expired CA still issues or returns certificates")
	}
}

func TestExclusiveWritePreservesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.pem")
	before := []byte("existing CA")
	writeFile(t, path, before)
	if err := writeExclusive(path, []byte("replacement"), 0o600); err == nil {
		t.Fatal("exclusive write overwrote file")
	}
	if !bytes.Equal(before, readFile(t, path)) {
		t.Fatal("existing file contents changed")
	}
}

func mustAuthority(t *testing.T, dir string) *Authority {
	t.Helper()
	a, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func encodeKey(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
