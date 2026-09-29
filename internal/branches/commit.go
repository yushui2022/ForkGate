package branches

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/yushui2022/ForkGate/internal/store"
)

// CommitItem is the public result for one staged write.
type CommitItem struct {
	ID             string `json:"staged_id"`
	Seq            int64  `json:"seq"`
	Status         string `json:"status"`
	ResponseStatus int    `json:"response_status,omitempty"`
	Error          string `json:"error,omitempty"`
}

type CommitResult struct {
	BranchID string       `json:"branch_id"`
	Status   string       `json:"status"`
	Items    []CommitItem `json:"items"`
}

type stagedRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Host    string      `json:"host"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
}

// Commit sends the selected branch's staged writes in sequence. It uses a
// direct transport so replay cannot recursively enter ForkGate's proxy. A
// failed response leaves the branch active and later staged writes untouched.
func (m *Manager) Commit(ctx context.Context, branchID string) (CommitResult, error) {
	m.operations.Lock()
	defer m.operations.Unlock()
	branch, err := m.GetBranch(ctx, branchID)
	if err != nil {
		return CommitResult{}, err
	}
	if branch.Mode != ModeSpeculative || branch.Status != StatusActive {
		return CommitResult{}, ErrInvalidTransition
	}
	if m.staging == nil {
		return CommitResult{}, ErrStagingUnavailable
	}
	writes, err := m.store.LoadStagedWrites(ctx, branchID)
	if err != nil {
		return CommitResult{}, err
	}
	result := CommitResult{BranchID: branchID, Status: "committed", Items: make([]CommitItem, 0, len(writes))}
	client := &http.Client{Timeout: 30 * time.Second, Transport: directTransport()}
	for _, write := range writes {
		item := CommitItem{ID: write.ID, Seq: write.Seq, Status: write.Status}
		if write.Status != "staged" {
			if write.Status == "succeeded" {
				result.Items = append(result.Items, item)
				continue
			}
			return result, fmt.Errorf("staged write %s is %s", write.ID, write.Status)
		}
		if err := m.store.SetStagedStatus(ctx, write.ID, "sending", 0); err != nil {
			return result, err
		}
		request, err := m.decodeStaged(branch, write)
		if err != nil {
			_ = m.store.SetStagedStatus(ctx, write.ID, "failed", 0)
			item.Status, item.Error = "failed", err.Error()
			result.Status = "partial"
			result.Items = append(result.Items, item)
			return result, err
		}
		response, err := client.Do(request.WithContext(ctx))
		if err != nil {
			_ = m.store.SetStagedStatus(ctx, write.ID, "unknown", 0)
			item.Status, item.Error = "unknown", err.Error()
			result.Status = "partial"
			result.Items = append(result.Items, item)
			return result, err
		}
		item.ResponseStatus = response.StatusCode
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_ = m.store.SetStagedStatus(ctx, write.ID, "failed", response.StatusCode)
			item.Status, item.Error = "failed", fmt.Sprintf("upstream returned %d", response.StatusCode)
			result.Status = "partial"
			result.Items = append(result.Items, item)
			return result, errors.New(item.Error)
		}
		if err := m.store.SetStagedStatus(ctx, write.ID, "succeeded", response.StatusCode); err != nil {
			return result, err
		}
		item.Status = "succeeded"
		result.Items = append(result.Items, item)
	}
	if err := m.store.CommitBranch(ctx, branchID); err != nil {
		return result, err
	}
	return result, nil
}

func directTransport() http.RoundTripper {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	clone := transport.Clone()
	clone.Proxy = nil
	return clone
}

func (m *Manager) decodeStaged(branch store.BranchRecord, write store.StagedWrite) (*http.Request, error) {
	aad := []byte("forkgate/staged/v1/" + branch.TreeID + "/" + branch.ID + "/" + write.ID)
	nonceSize := m.staging.aead.NonceSize()
	if len(write.RequestBlob) < nonceSize {
		return nil, errors.New("staged request payload is truncated")
	}
	plain, err := m.staging.aead.Open(nil, write.RequestBlob[:nonceSize], write.RequestBlob[nonceSize:], aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt staged write: %w", err)
	}
	var payload stagedRequest
	if err := json.Unmarshal(plain, &payload); err != nil {
		return nil, fmt.Errorf("decode staged write: %w", err)
	}
	request, err := http.NewRequest(payload.Method, payload.URL, bytes.NewReader(payload.Body))
	if err != nil {
		return nil, fmt.Errorf("rebuild staged request: %w", err)
	}
	request.Host = payload.Host
	request.Header = payload.Headers
	return request, nil
}
