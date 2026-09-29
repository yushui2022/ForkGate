package secretguard

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

func TestBase64VariantsFindEmbeddedSecrets(t *testing.T) {
	secret := []byte("ghp_1234567890abcdef")
	for prefixLen := 0; prefixLen < 10; prefixLen++ {
		prefix := bytes.Repeat([]byte{'p'}, prefixLen)
		suffix := bytes.Repeat([]byte{'s'}, 7)
		payload := append(append(append([]byte(nil), prefix...), secret...), suffix...)
		encoded := base64.StdEncoding.EncodeToString(payload)
		guard := New()
		if err := guard.Register(SecretSpec{Name: "TOKEN", Value: string(secret)}); err != nil {
			t.Fatal(err)
		}
		result := guard.Scan(Request{Host: "example.test", Body: []byte(encoded)})
		if !result.Blocked || !hasEncoding(result.Findings, "base64") {
			t.Fatalf("prefix=%d: result=%+v", prefixLen, result)
		}
	}
}

func TestEncodingPropertyTenThousandCases(t *testing.T) {
	rng := rand.New(rand.NewSource(20260929))
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-."
	for i := 0; i < 10000; i++ {
		length := 12 + rng.Intn(53)
		secretBytes := make([]byte, length)
		for j := range secretBytes {
			secretBytes[j] = alphabet[rng.Intn(len(alphabet))]
		}
		secret := string(secretBytes)
		prefix := randomASCII(rng, rng.Intn(51))
		suffix := randomASCII(rng, rng.Intn(51))
		payload := append(append([]byte(prefix), secretBytes...), []byte(suffix)...)
		mode := i % 8
		request := Request{Host: "example.test"}
		switch mode {
		case 0:
			request.Body = payload
		case 1:
			request.Body = []byte(base64.StdEncoding.EncodeToString(payload))
		case 2:
			request.Body = []byte(base64.RawURLEncoding.EncodeToString(payload))
		case 3:
			request.Body = []byte(hex.EncodeToString(payload))
		case 4:
			request.Body = []byte(strings.ToUpper(hex.EncodeToString(payload)))
		case 5:
			request.Body = []byte(url.QueryEscape(string(payload)))
			request.ContentType = "application/x-www-form-urlencoded"
		case 6:
			request.Body = []byte(fmt.Sprintf(`{"payload":%q}`, base64.StdEncoding.EncodeToString(payload)))
			request.ContentType = "application/json"
		case 7:
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			_, _ = writer.Write(payload)
			_ = writer.Close()
			request.Body = compressed.Bytes()
			request.ContentEncoding = "gzip"
		}
		guard := New()
		if err := guard.Register(SecretSpec{Name: "TOKEN", Value: secret}); err != nil {
			t.Fatal(err)
		}
		result := guard.Scan(request)
		if !result.Blocked {
			t.Fatalf("case %d mode %d was not detected: %+v", i, mode, result)
		}
	}
}

func randomASCII(rng *rand.Rand, length int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(result)
}

func TestBase64URLAndHexVariantsFindEmbeddedSecrets(t *testing.T) {
	secret := []byte{0xfb, 0xef, 0xfa, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x10, 0x20, 0x30, 0x40}
	guard := New()
	if err := guard.Register(SecretSpec{Name: "TOKEN", Value: string(secret)}); err != nil {
		t.Fatal(err)
	}
	urlEncoded := base64Variant(secret, 0, true)
	result := guard.Scan(Request{Host: "example.test", Body: []byte(urlEncoded)})
	if !result.Blocked || !hasEncoding(result.Findings, "base64url") {
		t.Fatalf("base64url result = %+v", result)
	}
	hexEncoded := ""
	for _, b := range secret {
		hexEncoded += strings.ToUpper(string([]byte{hexDigit(b >> 4), hexDigit(b & 0x0f)}))
	}
	result = guard.Scan(Request{Host: "example.test", Body: []byte(hexEncoded)})
	if !result.Blocked || !hasEncoding(result.Findings, "hex_upper") {
		t.Fatalf("hex result = %+v", result)
	}
}

func hexDigit(v byte) byte {
	if v < 10 {
		return '0' + v
	}
	return 'A' + v - 10
}

func TestShortSecretOnlyMatchesRaw(t *testing.T) {
	guard := New()
	if err := guard.Register(SecretSpec{Name: "SHORT", Value: "abc123"}); err != nil {
		t.Fatal(err)
	}
	result := guard.Scan(Request{Host: "example.test", Body: []byte("YWJjMTIz")})
	if result.Blocked {
		t.Fatalf("short encoded secret should not match: %+v", result)
	}
	result = guard.Scan(Request{Host: "example.test", Body: []byte("abc123")})
	if !result.Blocked || !hasEncoding(result.Findings, "raw") {
		t.Fatalf("raw short secret was not blocked: %+v", result)
	}
}

func TestAllowRuleCanRestrictLocation(t *testing.T) {
	secret := "ghp_1234567890abcdef"
	guard := New()
	if err := guard.Register(SecretSpec{
		Name:  "TOKEN",
		Value: secret,
		Allow: []AllowRule{{Host: "api.github.com", Locations: []string{"header:Authorization"}}},
	}); err != nil {
		t.Fatal(err)
	}
	allowed := guard.Scan(Request{
		Host:    "api.github.com:443",
		Headers: http.Header{"Authorization": []string{secret}},
	})
	if allowed.Blocked || len(allowed.Findings) == 0 || !allowed.Findings[0].Allowed {
		t.Fatalf("allowed header was blocked: %+v", allowed)
	}
	blocked := guard.Scan(Request{Host: "api.github.com", Body: []byte(secret)})
	if !blocked.Blocked || len(blocked.Findings) == 0 || blocked.Findings[0].Allowed {
		t.Fatalf("body should be blocked: %+v", blocked)
	}
}

func TestNormalizedGzipAndJSONViews(t *testing.T) {
	secret := "ghp_1234567890abcdef"
	guard := New()
	if err := guard.Register(SecretSpec{Name: "TOKEN", Value: secret}); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte(`{"token":"ghp_1234567890abcdef"}`))
	_ = writer.Close()
	result := guard.Scan(Request{
		Host:            "example.test",
		ContentType:     "application/json",
		ContentEncoding: "gzip",
		Body:            compressed.Bytes(),
	})
	if !result.Blocked {
		t.Fatalf("gzip/json secret was not blocked: %+v", result)
	}
}

func TestDeflateBrotliAndUnknownEncodingFailClosed(t *testing.T) {
	secret := "ghp_1234567890abcdef"
	guard := New()
	if err := guard.Register(SecretSpec{Name: "TOKEN", Value: secret}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		encoding string
		compress func(*bytes.Buffer, []byte) error
	}{
		{name: "zlib deflate", encoding: "deflate", compress: func(out *bytes.Buffer, body []byte) error {
			w := zlib.NewWriter(out)
			_, err := w.Write(body)
			if closeErr := w.Close(); err == nil {
				err = closeErr
			}
			return err
		}},
		{name: "brotli", encoding: "br", compress: func(out *bytes.Buffer, body []byte) error {
			w := brotli.NewWriter(out)
			_, err := w.Write(body)
			if closeErr := w.Close(); err == nil {
				err = closeErr
			}
			return err
		}},
	} {
		var compressed bytes.Buffer
		if err := tc.compress(&compressed, []byte(secret)); err != nil {
			t.Fatal(tc.name, err)
		}
		result := guard.Scan(Request{Host: "example.test", ContentEncoding: tc.encoding, Body: compressed.Bytes()})
		if !result.Blocked {
			t.Fatalf("%s was not blocked: %+v", tc.name, result)
		}
	}
	unknown := guard.Scan(Request{Host: "example.test", ContentEncoding: "zstd", Body: []byte(secret)})
	if !unknown.Blocked || !unknown.ScanError {
		t.Fatalf("unknown encoding was not fail-closed: %+v", unknown)
	}
}

func TestFlagAndOversizePolicy(t *testing.T) {
	secret := "ghp_1234567890abcdef"
	guard := New()
	if err := guard.Register(SecretSpec{Name: "TOKEN", Value: secret, Action: ActionFlag}); err != nil {
		t.Fatal(err)
	}
	result := guard.Scan(Request{Host: "example.test", Body: []byte(secret)})
	if result.Blocked || !result.Flagged {
		t.Fatalf("flag action result = %+v", result)
	}
	if err := guard.SetBodyPolicy(4, ActionBlock); err != nil {
		t.Fatal(err)
	}
	result = guard.Scan(Request{Host: "example.test", Body: []byte(strings.Repeat("x", 5))})
	if !result.Oversize || !result.Blocked {
		t.Fatalf("oversize result = %+v", result)
	}
}

func hasEncoding(findings []Finding, encoding string) bool {
	for _, finding := range findings {
		if finding.Encoding == encoding {
			return true
		}
	}
	return false
}

func BenchmarkScan1MB100Secrets(b *testing.B) {
	guard, body := benchmarkFixture()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result := guard.Scan(Request{Host: "example.test", Body: body})
		if result.Blocked {
			b.Fatal("benchmark body unexpectedly matched a secret")
		}
	}
}

func TestScanP95Sample(t *testing.T) {
	guard, body := benchmarkFixture()
	times := make([]time.Duration, 25)
	for i := range times {
		started := time.Now()
		result := guard.Scan(Request{Host: "example.test", Body: body})
		if result.Blocked {
			t.Fatal("benchmark body unexpectedly matched a secret")
		}
		times[i] = time.Since(started)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	t.Logf("1MiB x 100 secrets: p50=%s p95=%s", times[len(times)/2], times[23])
}

func benchmarkFixture() (*Guard, []byte) {
	guard := New()
	for i := 0; i < 100; i++ {
		_ = guard.Register(SecretSpec{Name: fmt.Sprintf("TOKEN_%03d", i), Value: fmt.Sprintf("secret-value-%03d-abcdefghijkl", i)})
	}
	body := bytes.Repeat([]byte("ordinary request body without registered secrets\n"), 22000)
	if len(body) > 1<<20 {
		body = body[:1<<20]
	}
	return guard, body
}
