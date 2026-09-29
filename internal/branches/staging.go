package branches

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/yushui2022/ForkGate/internal/store"
)

const MaxStagedBody = 10 << 20

var (
	ErrStagingUnavailable = errors.New("staging requires a master key")
	ErrBodyTooLarge       = errors.New("request body exceeds staging limit")
)

type stagingCipher struct{ aead cipher.AEAD }

// SetStagingKey is called once before serving requests. Request credentials,
// URLs and bodies are persisted only inside the authenticated ciphertext.
func (m *Manager) SetStagingKey(key []byte) error {
	if len(key) != 32 {
		return ErrStagingUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	m.staging = &stagingCipher{aead: aead}
	return nil
}

func (m *Manager) Stage(ctx context.Context, branch store.BranchRecord, r *http.Request) (store.StagedWrite, error) {
	if m.staging == nil {
		return store.StagedWrite{}, ErrStagingUnavailable
	}
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxStagedBody+1))
		_ = r.Body.Close()
		if err != nil {
			return store.StagedWrite{}, err
		}
	}
	if len(body) > MaxStagedBody {
		return store.StagedWrite{}, ErrBodyTooLarge
	}
	id, err := randomID("sw_")
	if err != nil {
		return store.StagedWrite{}, err
	}
	headers := r.Header.Clone()
	headers.Del("Proxy-Authorization")
	request, err := json.Marshal(struct {
		Method  string      `json:"method"`
		URL     string      `json:"url"`
		Host    string      `json:"host"`
		Headers http.Header `json:"headers"`
		Body    []byte      `json:"body"`
	}{r.Method, r.URL.String(), r.Host, headers, body})
	if err != nil {
		return store.StagedWrite{}, err
	}
	nonce := make([]byte, m.staging.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return store.StagedWrite{}, err
	}
	// Version/domain separation prevents a ciphertext from being reused as a
	// secret value or as a staged request belonging to a different branch.
	aad := []byte("forkgate/staged/v1/" + branch.TreeID + "/" + branch.ID + "/" + id)
	record := store.StagedWrite{
		ID: id, BranchID: branch.ID, Method: r.Method, Host: r.URL.Host,
		Status: "staged", CreatedAt: time.Now().UTC(),
		RequestBlob: m.staging.aead.Seal(nonce, nonce, request, aad),
	}
	if err := m.store.SaveStagedWrite(ctx, &record); err != nil {
		return store.StagedWrite{}, err
	}
	record.RequestBlob = nil
	return record, nil
}

func (m *Manager) ListStaged(ctx context.Context, branchID string) ([]store.StagedWrite, error) {
	if _, err := m.GetBranch(ctx, branchID); err != nil {
		return nil, err
	}
	return m.store.ListStagedWrites(ctx, branchID)
}

func ShouldStage(branch *store.BranchRecord, method string) bool {
	if branch == nil || branch.Mode != ModeSpeculative {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
