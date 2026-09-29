package branches

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yushui2022/ForkGate/internal/store"
)

const (
	ModeLive        = "live"
	ModeSpeculative = "speculative"

	StatusActive    = "active"
	StatusSealed    = "sealed"
	StatusCommitted = "committed"
	StatusAborted   = "aborted"

	defaultForkLimit = 32
)

var (
	ErrInvalidToken      = errors.New("invalid branch token")
	ErrBranchSealed      = errors.New("branch token is sealed")
	ErrBranchTerminal    = errors.New("branch is terminal")
	ErrInvalidTransition = errors.New("invalid branch state transition")
	ErrForkLimit         = errors.New("invalid fork count")
	ErrNestedFork        = errors.New("nested fork is not supported")
)

type Token struct {
	BranchID string `json:"branch_id"`
	Token    string `json:"token"`
}

type CreatedTree struct {
	Tree      store.TreeRecord
	Root      store.BranchRecord
	RootToken string
}

type ForkResult struct {
	ParentID string  `json:"parent_id"`
	Children []Token `json:"children"`
}

// Manager owns the branch state machine. Tokens are returned only by create
// and fork operations; the store receives only their SHA-256 hashes.
type Manager struct {
	store      *store.Store
	forkLimit  int
	operations sync.RWMutex
	onInactive func(string)
	staging    *stagingCipher
}

func New(s *store.Store) *Manager {
	return &Manager{store: s, forkLimit: defaultForkLimit}
}

func (m *Manager) SetForkLimit(limit int) {
	m.operations.Lock()
	defer m.operations.Unlock()
	if limit > 0 {
		m.forkLimit = limit
	}
}

// SetOnInactive is configured before serving requests. The callback closes
// idle branch connections after a successful state change.
func (m *Manager) SetOnInactive(callback func(string)) { m.onInactive = callback }

func (m *Manager) CreateTree(ctx context.Context) (CreatedTree, error) {
	treeID, err := randomID("tr_")
	if err != nil {
		return CreatedTree{}, err
	}
	branchID, err := randomID("br_")
	if err != nil {
		return CreatedTree{}, err
	}
	token, hash, err := newToken()
	if err != nil {
		return CreatedTree{}, err
	}
	now := time.Now().UTC()
	tree := store.TreeRecord{ID: treeID, RootBranch: branchID, CreatedAt: now}
	root := store.BranchRecord{
		ID: branchID, TreeID: treeID, Mode: ModeLive, Status: StatusActive,
		TokenHash: hash, CreatedAt: now,
	}
	if err := m.store.CreateTree(ctx, tree, root); err != nil {
		return CreatedTree{}, err
	}
	return CreatedTree{Tree: tree, Root: root, RootToken: token}, nil
}

func (m *Manager) GetTree(ctx context.Context, id string) (store.TreeRecord, []store.BranchRecord, error) {
	tree, err := m.store.GetTree(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.TreeRecord{}, nil, fmt.Errorf("tree %q: %w", id, sql.ErrNoRows)
		}
		return store.TreeRecord{}, nil, err
	}
	branches, err := m.store.ListBranches(ctx, id)
	if err != nil {
		return store.TreeRecord{}, nil, err
	}
	return tree, branches, nil
}

func (m *Manager) GetBranch(ctx context.Context, id string) (store.BranchRecord, error) {
	branch, err := m.store.GetBranch(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return store.BranchRecord{}, fmt.Errorf("branch %q: %w", id, sql.ErrNoRows)
	}
	return branch, err
}

// RegisterFork records children that an external sandbox backend has already
// created. ForkGate never copies a filesystem, memory image, process, or
// network namespace; it only seals the parent and issues child identities.
func (m *Manager) RegisterFork(ctx context.Context, parentID string, count int) (ForkResult, error) {
	m.operations.Lock()
	defer m.operations.Unlock()
	if count < 1 || count > m.forkLimit {
		return ForkResult{}, ErrForkLimit
	}
	parent, err := m.GetBranch(ctx, parentID)
	if err != nil {
		return ForkResult{}, err
	}
	if parent.Status != StatusActive {
		return ForkResult{}, ErrInvalidTransition
	}
	if parent.Mode != ModeLive {
		return ForkResult{}, ErrNestedFork
	}

	children := make([]store.BranchRecord, 0, count)
	tokens := make([]Token, 0, count)
	now := time.Now().UTC()
	for i := 0; i < count; i++ {
		childID, err := randomID("br_")
		if err != nil {
			return ForkResult{}, err
		}
		token, hash, err := newToken()
		if err != nil {
			return ForkResult{}, err
		}
		children = append(children, store.BranchRecord{
			ID: childID, TreeID: parent.TreeID, ParentID: parent.ID,
			Mode: ModeSpeculative, Status: StatusActive, TokenHash: hash, CreatedAt: now,
		})
		tokens = append(tokens, Token{BranchID: childID, Token: token})
	}
	if err := m.store.ForkBranch(ctx, parent.ID, children); err != nil {
		if errors.Is(err, store.ErrInvalidTransition) {
			return ForkResult{}, ErrInvalidTransition
		}
		return ForkResult{}, err
	}
	if m.onInactive != nil {
		m.onInactive(parent.ID)
	}
	return ForkResult{ParentID: parent.ID, Children: tokens}, nil
}

func (m *Manager) Authenticate(ctx context.Context, token string) (store.BranchRecord, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 512 {
		return store.BranchRecord{}, ErrInvalidToken
	}
	hash := tokenHash(token)
	branch, err := m.store.FindBranchByTokenHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return store.BranchRecord{}, ErrInvalidToken
	}
	if err != nil {
		return store.BranchRecord{}, err
	}
	// The hash lookup is already exact; keep the constant-time comparison as
	// an explicit guard against accidental future lookup changes.
	stored, err := hex.DecodeString(branch.TokenHash)
	if err != nil || subtle.ConstantTimeCompare(stored, hashBytes(token)) != 1 {
		return store.BranchRecord{}, ErrInvalidToken
	}
	return branch, m.active(ctx, branch)
}

func (m *Manager) active(ctx context.Context, branch store.BranchRecord) error {
	switch branch.Status {
	case StatusActive:
		return nil
	case StatusSealed:
		_ = m.store.RecordEvent(ctx, branch.TreeID, branch.ID, "identity.stale", map[string]any{"status": StatusSealed})
		return ErrBranchSealed
	case StatusCommitted, StatusAborted:
		return ErrBranchTerminal
	default:
		return ErrInvalidTransition
	}
}

func (m *Manager) Abort(ctx context.Context, branchID string) error {
	m.operations.Lock()
	defer m.operations.Unlock()
	branch, err := m.GetBranch(ctx, branchID)
	if err != nil {
		return err
	}
	if branch.Status != StatusActive {
		return ErrInvalidTransition
	}
	if err := m.store.AbortBranch(ctx, branchID); err != nil {
		if errors.Is(err, store.ErrInvalidTransition) {
			return ErrInvalidTransition
		}
		return err
	}
	if m.onInactive != nil {
		m.onInactive(branch.ID)
	}
	return nil
}

// Acquire keeps a forwarded request ahead of a concurrent fork/abort. Release
// before waiting for the next CONNECT request; idle tunnels must not block fork.
func (m *Manager) Acquire(ctx context.Context, token string) (store.BranchRecord, func(), error) {
	m.operations.RLock()
	branch, err := m.Authenticate(ctx, token)
	return m.lease(branch, err)
}

// AcquireBranch rechecks the identity inherited from CONNECT for the inner
// HTTP request. Inner headers can never select a different branch.
func (m *Manager) AcquireBranch(ctx context.Context, id string) (store.BranchRecord, func(), error) {
	m.operations.RLock()
	branch, err := m.GetBranch(ctx, id)
	if err == nil {
		err = m.active(ctx, branch)
	}
	return m.lease(branch, err)
}

func (m *Manager) lease(branch store.BranchRecord, err error) (store.BranchRecord, func(), error) {
	var once sync.Once
	release := func() { once.Do(m.operations.RUnlock) }
	if err != nil {
		release()
	}
	return branch, release, err
}

func newToken() (string, string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", fmt.Errorf("generate branch token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	return token, tokenHash(token), nil
}

func randomID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func tokenHash(token string) string {
	return hex.EncodeToString(hashBytes(token))
}

func hashBytes(token string) []byte {
	hash := sha256.Sum256([]byte(token))
	return hash[:]
}
