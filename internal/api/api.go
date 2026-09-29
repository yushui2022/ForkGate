package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yushui2022/ForkGate/internal/branches"
	"github.com/yushui2022/ForkGate/internal/mitm"
	"github.com/yushui2022/ForkGate/internal/secretguard"
	"github.com/yushui2022/ForkGate/internal/secrets"
	"github.com/yushui2022/ForkGate/internal/store"
)

type Server struct {
	store      *store.Store
	ca         *mitm.Authority
	adminToken string
	secretSvc  *secrets.Service
	branchSvc  *branches.Manager
}

func (s *Server) SetSecretService(service *secrets.Service) {
	s.secretSvc = service
}

func (s *Server) SetBranchManager(manager *branches.Manager) {
	s.branchSvc = manager
}

func New(s *store.Store, ca *mitm.Authority, adminToken string) *Server {
	return &Server{store: s, ca: ca, adminToken: adminToken}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", s.auth(http.HandlerFunc(s.healthz)))
	mux.Handle("GET /v1/ca.pem", s.auth(http.HandlerFunc(s.caPEM)))
	mux.Handle("GET /metrics", s.auth(http.HandlerFunc(s.metrics)))
	mux.Handle("GET /v1/events", s.auth(http.HandlerFunc(s.events)))
	mux.Handle("POST /v1/secrets", s.auth(http.HandlerFunc(s.createSecret)))
	mux.Handle("GET /v1/secrets", s.auth(http.HandlerFunc(s.listSecrets)))
	mux.Handle("DELETE /v1/secrets/{name}", s.auth(http.HandlerFunc(s.deleteSecret)))
	mux.Handle("POST /v1/trees", s.auth(http.HandlerFunc(s.createTree)))
	mux.Handle("GET /v1/trees/{tree_id}", s.auth(http.HandlerFunc(s.getTree)))
	mux.Handle("POST /v1/branches/{branch_id}/fork", s.auth(http.HandlerFunc(s.forkBranch)))
	mux.Handle("POST /v1/branches/{branch_id}/commit", s.auth(http.HandlerFunc(s.commitBranch)))
	mux.Handle("POST /v1/branches/{branch_id}/abort", s.auth(http.HandlerFunc(s.abortBranch)))
	mux.Handle("GET /v1/branches/{branch_id}/staged", s.auth(http.HandlerFunc(s.listStaged)))
	return mux
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminToken == "" {
			unauthorized(w)
			return
		}
		const prefix = "Bearer "
		header := r.Header.Get("Authorization")
		provided := ""
		if strings.HasPrefix(header, prefix) {
			provided = strings.TrimSpace(strings.TrimPrefix(header, prefix))
		}
		providedHash := sha256.Sum256([]byte(provided))
		expectedHash := sha256.Sum256([]byte(s.adminToken))
		if subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) != 1 {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="forkgate"`)
	http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) caPEM(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(s.ca.CAPEM())
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	count, err := s.store.EventCount(r.Context())
	if err != nil {
		http.Error(w, "metrics unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte("# TYPE forkgate_events_total counter\n"))
	_, _ = w.Write([]byte("forkgate_events_total " + strconv.FormatInt(count, 10) + "\n"))
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	since, limit, err := eventQuery(r)
	if err != nil {
		http.Error(w, `{"error":"invalid_event_query"}`, http.StatusBadRequest)
		return
	}
	events, err := s.store.Events(r.Context(), since, limit)
	if err != nil {
		http.Error(w, "events unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

type secretRequest struct {
	Name   string                  `json:"name"`
	Value  string                  `json:"value"`
	Allow  []secretguard.AllowRule `json:"allow"`
	Action secretguard.Action      `json:"action"`
}

func (s *Server) createSecret(w http.ResponseWriter, r *http.Request) {
	if s.secretSvc == nil {
		http.Error(w, "secretguard unavailable", http.StatusServiceUnavailable)
		return
	}
	var request secretRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, `{"error":"invalid_secret_request"}`, http.StatusBadRequest)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		http.Error(w, `{"error":"invalid_secret_request"}`, http.StatusBadRequest)
		return
	}
	if err := s.secretSvc.Register(r.Context(), secretguard.SecretSpec{
		Name: request.Name, Value: request.Value, Allow: request.Allow, Action: request.Action,
	}); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			http.Error(w, `{"error":"secret_exists"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"invalid_secret_request"}`, http.StatusBadRequest)
		return
	}
	for _, summary := range s.secretSvc.List(r.Context()) {
		if summary.Name == request.Name {
			writeJSON(w, http.StatusCreated, summary)
			return
		}
	}
	http.Error(w, "secret unavailable", http.StatusInternalServerError)
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	if s.secretSvc == nil {
		http.Error(w, "secretguard unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, s.secretSvc.List(r.Context()))
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	if s.secretSvc == nil {
		http.Error(w, "secretguard unavailable", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, `{"error":"invalid_secret_name"}`, http.StatusBadRequest)
		return
	}
	if err := s.secretSvc.Delete(r.Context(), name); err != nil {
		http.Error(w, "secret delete failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type branchSummary struct {
	ID             string     `json:"branch_id"`
	ParentID       string     `json:"parent_id,omitempty"`
	Mode           string     `json:"mode"`
	Status         string     `json:"status"`
	DivergentCount int64      `json:"divergent_count"`
	CreatedAt      time.Time  `json:"created_at"`
	SealedAt       *time.Time `json:"sealed_at,omitempty"`
}

func publicBranch(branch store.BranchRecord) branchSummary {
	return branchSummary{
		ID: branch.ID, ParentID: branch.ParentID, Mode: branch.Mode, Status: branch.Status,
		DivergentCount: branch.DivergentCount, CreatedAt: branch.CreatedAt, SealedAt: branch.SealedAt,
	}
}

func (s *Server) createTree(w http.ResponseWriter, r *http.Request) {
	if s.branchSvc == nil {
		http.Error(w, `{"error":"branches_unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	created, err := s.branchSvc.CreateTree(r.Context())
	if err != nil {
		http.Error(w, `{"error":"tree_create_failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"tree_id":     created.Tree.ID,
		"root_branch": publicBranch(created.Root),
		"token":       created.RootToken,
	})
}

func (s *Server) getTree(w http.ResponseWriter, r *http.Request) {
	if s.branchSvc == nil {
		http.Error(w, `{"error":"branches_unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	tree, branchesList, err := s.branchSvc.GetTree(r.Context(), r.PathValue("tree_id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, `{"error":"tree_not_found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"tree_unavailable"}`, http.StatusInternalServerError)
		return
	}
	publicBranches := make([]branchSummary, 0, len(branchesList))
	for _, branch := range branchesList {
		publicBranches = append(publicBranches, publicBranch(branch))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tree_id": tree.ID, "root_branch": tree.RootBranch, "created_at": tree.CreatedAt, "branches": publicBranches,
	})
}

type forkRequest struct {
	Count int `json:"count"`
}

func (s *Server) forkBranch(w http.ResponseWriter, r *http.Request) {
	if s.branchSvc == nil {
		http.Error(w, `{"error":"branches_unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	var request forkRequest
	if err := decodeJSON(r, w, &request, 8<<10); err != nil {
		http.Error(w, `{"error":"invalid_fork_request"}`, http.StatusBadRequest)
		return
	}
	result, err := s.branchSvc.RegisterFork(r.Context(), r.PathValue("branch_id"), request.Count)
	if err != nil {
		switch {
		case errors.Is(err, branches.ErrForkLimit):
			http.Error(w, `{"error":"invalid_fork_count"}`, http.StatusBadRequest)
		case errors.Is(err, branches.ErrNestedFork), errors.Is(err, branches.ErrInvalidTransition):
			http.Error(w, `{"error":"branch_not_forkable"}`, http.StatusConflict)
		case errors.Is(err, sql.ErrNoRows):
			http.Error(w, `{"error":"branch_not_found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"error":"branch_fork_failed"}`, http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) abortBranch(w http.ResponseWriter, r *http.Request) {
	if s.branchSvc == nil {
		http.Error(w, `{"error":"branches_unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if err := s.branchSvc.Abort(r.Context(), r.PathValue("branch_id")); err != nil {
		switch {
		case errors.Is(err, branches.ErrInvalidTransition):
			http.Error(w, `{"error":"branch_not_abortable"}`, http.StatusConflict)
		case errors.Is(err, sql.ErrNoRows):
			http.Error(w, `{"error":"branch_not_found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"error":"branch_abort_failed"}`, http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) commitBranch(w http.ResponseWriter, r *http.Request) {
	if s.branchSvc == nil {
		http.Error(w, `{"error":"branches_unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	result, err := s.branchSvc.Commit(r.Context(), r.PathValue("branch_id"))
	if err != nil {
		switch {
		case errors.Is(err, branches.ErrInvalidTransition):
			http.Error(w, `{"error":"branch_not_committable"}`, http.StatusConflict)
		case errors.Is(err, branches.ErrStagingUnavailable):
			http.Error(w, `{"error":"staging_unavailable"}`, http.StatusServiceUnavailable)
		case errors.Is(err, sql.ErrNoRows):
			http.Error(w, `{"error":"branch_not_found"}`, http.StatusNotFound)
		default:
			if result.Status == "partial" {
				writeJSON(w, http.StatusBadGateway, result)
				return
			}
			http.Error(w, `{"error":"branch_commit_failed"}`, http.StatusBadGateway)
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type stagedSummary struct {
	ID        string    `json:"staged_id"`
	BranchID  string    `json:"branch_id"`
	Seq       int64     `json:"seq"`
	Method    string    `json:"method"`
	Host      string    `json:"host"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) listStaged(w http.ResponseWriter, r *http.Request) {
	if s.branchSvc == nil {
		http.Error(w, `{"error":"branches_unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	items, err := s.branchSvc.ListStaged(r.Context(), r.PathValue("branch_id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, `{"error":"branch_not_found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"staged_unavailable"}`, http.StatusInternalServerError)
		return
	}
	summaries := make([]stagedSummary, 0, len(items))
	for _, item := range items {
		summaries = append(summaries, stagedSummary{
			ID: item.ID, BranchID: item.BranchID, Seq: item.Seq, Method: item.Method,
			Host: item.Host, Status: item.Status, CreatedAt: item.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"staged": summaries})
}

func decodeJSON(r *http.Request, w http.ResponseWriter, target any, limit int64) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing json")
	}
	return nil
}

func eventQuery(r *http.Request) (int64, int64, error) {
	since := int64(0)
	if raw := r.URL.Query().Get("since"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			return 0, 0, errInvalidEventQuery
		}
		since = value
	}
	limit := int64(100)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 1 || value > 1000 {
			return 0, 0, errInvalidEventQuery
		}
		limit = value
	}
	return since, limit, nil
}

var errInvalidEventQuery = errors.New("invalid event query")

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
