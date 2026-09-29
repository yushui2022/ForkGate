package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yushui2022/ForkGate/internal/mitm"
	"github.com/yushui2022/ForkGate/internal/secretguard"
	"github.com/yushui2022/ForkGate/internal/secrets"
	"github.com/yushui2022/ForkGate/internal/store"
)

func TestHealthAndAuth(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/api.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ca, err := mitm.LoadOrCreate(t.TempDir() + "/ca")
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, ca, "secret").Handler()

	health := httptest.NewRecorder()
	h.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated health status = %d", health.Code)
	}
	authHealthReq := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	authHealthReq.Header.Set("Authorization", "Bearer secret")
	authHealth := httptest.NewRecorder()
	h.ServeHTTP(authHealth, authHealthReq)
	if authHealth.Code != http.StatusOK {
		t.Fatalf("authenticated health status = %d", authHealth.Code)
	}

	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/v1/ca.pem", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d", unauth.Code)
	}

	authReq := httptest.NewRequest(http.MethodGet, "/v1/ca.pem", nil)
	authReq.Header.Set("Authorization", "Bearer secret")
	auth := httptest.NewRecorder()
	h.ServeHTTP(auth, authReq)
	if auth.Code != http.StatusOK || !strings.Contains(auth.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("CA response = %d %q", auth.Code, auth.Body.String())
	}
}

func TestEmptyAdminTokenAlwaysUnauthorized(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/api.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ca, err := mitm.LoadOrCreate(t.TempDir() + "/ca")
	if err != nil {
		t.Fatal(err)
	}
	h := New(s, ca, "").Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer ")
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("empty-token status = %d", got.Code)
	}
}

func TestEventsEndpointAuthAndCursor(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/api.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ca, err := mitm.LoadOrCreate(t.TempDir() + "/ca")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEvent(t.Context(), "tree", "branch", "request.forwarded", map[string]any{"status": 200}); err != nil {
		t.Fatal(err)
	}
	h := New(s, ca, "secret").Handler()

	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated events status = %d", unauth.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/events?since=-1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, req)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid since status = %d", bad.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/events?since=0&limit=1", nil)
	req.Header.Set("Authorization", "Bearer secret")
	got := httptest.NewRecorder()
	h.ServeHTTP(got, req)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"id":1`) {
		t.Fatalf("events response = %d %q", got.Code, got.Body.String())
	}
}

func TestSecretsAPIOnlyReturnsSummaries(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/api.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ca, err := mitm.LoadOrCreate(t.TempDir() + "/ca")
	if err != nil {
		t.Fatal(err)
	}
	guard := secretguard.New()
	service, err := secrets.New(t.Context(), s, guard, []byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	server := New(s, ca, "secret")
	server.SetSecretService(service)
	h := server.Handler()
	body := strings.NewReader(`{"name":"TOKEN","value":"ghp_1234567890abcdef","allow":[{"host":"api.github.com","locations":["header:Authorization"]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets", body)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	h.ServeHTTP(created, req)
	if created.Code != http.StatusCreated || strings.Contains(created.Body.String(), "ghp_1234567890abcdef") {
		t.Fatalf("create response = %d %q", created.Code, created.Body.String())
	}
	duplicate := httptest.NewRecorder()
	duplicateReq := httptest.NewRequest(http.MethodPost, "/v1/secrets", strings.NewReader(`{"name":"TOKEN","value":"other"}`))
	duplicateReq.Header.Set("Authorization", "Bearer secret")
	duplicateReq.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(duplicate, duplicateReq)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d", duplicate.Code)
	}
	invalid := httptest.NewRecorder()
	invalidReq := httptest.NewRequest(http.MethodPost, "/v1/secrets", strings.NewReader(`{"name":"OTHER","value":"other"} {"extra":true}`))
	invalidReq.Header.Set("Authorization", "Bearer secret")
	invalidReq.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(invalid, invalidReq)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status = %d", invalid.Code)
	}
	listReq := httptest.NewRequest(http.MethodGet, "/v1/secrets", nil)
	listReq.Header.Set("Authorization", "Bearer secret")
	listed := httptest.NewRecorder()
	h.ServeHTTP(listed, listReq)
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), "ghp_1234567890abcdef") || !strings.Contains(listed.Body.String(), "TOKEN") {
		t.Fatalf("list response = %d %q", listed.Code, listed.Body.String())
	}
	deleteReq := httptest.NewRequest(http.MethodDelete, "/v1/secrets/TOKEN", nil)
	deleteReq.Header.Set("Authorization", "Bearer secret")
	deleted := httptest.NewRecorder()
	h.ServeHTTP(deleted, deleteReq)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", deleted.Code)
	}
}
