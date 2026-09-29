package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yushui2022/ForkGate/internal/mitm"
	"github.com/yushui2022/ForkGate/internal/secretguard"
	"github.com/yushui2022/ForkGate/internal/store"
)

func TestExplicitHTTPProxyForwardsAndRecords(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "ok")
		_, _ = w.Write([]byte("hello"))
	}))
	defer upstream.Close()

	s, err := store.Open(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ca, err := mitm.LoadOrCreate(t.TempDir() + "/ca")
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(New(s, ca))
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	resp, err := client.Get(upstream.URL + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" || resp.Header.Get("X-Upstream") != "ok" {
		t.Fatalf("proxy response = %d %q", resp.StatusCode, body)
	}
	count, err := s.EventCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("event count = %d, want 1", count)
	}
}

func TestConnectMITMForwardsHTTPS(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secure hello"))
	}))
	defer upstream.Close()

	s, err := store.Open(t.TempDir() + "/connect.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ca, err := mitm.LoadOrCreate(t.TempDir() + "/ca")
	if err != nil {
		t.Fatal(err)
	}
	p := New(s, ca)
	// The fake upstream uses a self-signed certificate, so the proxy's
	// upstream transport trusts the test server explicitly.
	p.transport = upstream.Client().Transport.(*http.Transport).Clone()
	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(ca.CAPEM())
	rootPool := x509.NewCertPool()
	rootCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	rootPool.AddCert(rootCert)
	client := &http.Client{Transport: &http.Transport{
		Proxy:           proxyURLFunc(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: rootPool},
	}}
	resp, err := client.Get(upstream.URL + "/secure")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "secure hello" {
		t.Fatalf("proxy response = %d %q", resp.StatusCode, body)
	}
	count, err := s.EventCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("event count = %d, want connect and forwarded events", count)
	}
}

func TestSecretGuardBlocksBeforeUpstream(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		_, _ = w.Write([]byte("should not be reached"))
	}))
	defer upstream.Close()

	s, err := store.Open(t.TempDir() + "/guard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ca, err := mitm.LoadOrCreate(t.TempDir() + "/ca")
	if err != nil {
		t.Fatal(err)
	}
	p := New(s, ca)
	guard := secretguard.New()
	if err := guard.Register(secretguard.SecretSpec{Name: "TOKEN", Value: "ghp_1234567890abcdef"}); err != nil {
		t.Fatal(err)
	}
	p.SetGuard(guard)
	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	request, err := http.NewRequest(http.MethodPost, upstream.URL+"/exfil", strings.NewReader("ghp_1234567890abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("upstream received %d blocked requests", upstreamHits.Load())
	}
	events, err := s.Events(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "secret.blocked" {
		t.Fatalf("events = %+v", events)
	}
}

func proxyURLFunc(u *url.URL) func(*http.Request) (*url.URL, error) {
	return func(*http.Request) (*url.URL, error) { return u, nil }
}
