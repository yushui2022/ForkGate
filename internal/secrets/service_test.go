package secrets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yushui2022/ForkGate/internal/secretguard"
	"github.com/yushui2022/ForkGate/internal/store"
)

func TestServiceEncryptsAndReloadsSecrets(t *testing.T) {
	path := t.TempDir() + "/secrets.db"
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte(strings.Repeat("k", 32))
	guard := secretguard.New()
	service, err := New(context.Background(), db, guard, key)
	if err != nil {
		t.Fatal(err)
	}
	value := "ghp_1234567890abcdef"
	if err := service.Register(context.Background(), secretguard.SecretSpec{
		Name: "GITHUB_TOKEN", Value: value,
		Allow: []secretguard.AllowRule{{Host: "api.github.com", Locations: []string{"header:Authorization"}}},
	}); err != nil {
		t.Fatal(err)
	}
	records, err := db.SecretRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || strings.Contains(string(records[0].ValueBlob), value) || records[0].AllowJSON == "" {
		t.Fatalf("secret record leaked or missing: %+v", records)
	}
	if len(service.List(context.Background())) != 1 {
		t.Fatal("secret summary missing")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		data, readErr := os.ReadFile(candidate)
		if readErr == nil && strings.Contains(string(data), value) {
			t.Fatalf("plaintext secret leaked into %s", filepath.Base(candidate))
		}
	}

	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reloadedGuard := secretguard.New()
	reloaded, err := New(context.Background(), db, reloadedGuard, key)
	if err != nil {
		t.Fatal(err)
	}
	result := reloadedGuard.Scan(secretguard.Request{Host: "example.test", Body: []byte(value)})
	if !result.Blocked {
		t.Fatalf("reloaded guard did not block secret: %+v", result)
	}
	if _, err := New(context.Background(), db, secretguard.New(), []byte(strings.Repeat("x", 32))); err == nil {
		t.Fatal("wrong key unexpectedly loaded secrets")
	}
	if err := reloaded.Delete(context.Background(), "GITHUB_TOKEN"); err != nil {
		t.Fatal(err)
	}
}

func TestParseMasterKey(t *testing.T) {
	if _, err := ParseMasterKey(""); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := ParseMasterKey(strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseMasterKey("too-short"); err == nil {
		t.Fatal("short key accepted")
	}
}
