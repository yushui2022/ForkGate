// Package secrets persists registered SecretGuard rules with AES-GCM encrypted
// values. The plaintext value is passed only to the in-memory matcher.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/yushui2022/ForkGate/internal/secretguard"
	"github.com/yushui2022/ForkGate/internal/store"
)

type Summary struct {
	Name   string                  `json:"name"`
	Allow  []secretguard.AllowRule `json:"allow,omitempty"`
	Action secretguard.Action      `json:"action"`
}

type Service struct {
	mu    sync.RWMutex
	store *store.Store
	guard *secretguard.Guard
	rules map[string]Summary
	aead  cipher.AEAD
}

func New(ctx context.Context, db *store.Store, guard *secretguard.Guard, key []byte) (*Service, error) {
	if db == nil || guard == nil {
		return nil, errors.New("secret service requires store and guard")
	}
	if len(key) != 32 {
		return nil, errors.New("secret master key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create secret cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create secret AEAD: %w", err)
	}
	service := &Service{store: db, guard: guard, rules: make(map[string]Summary), aead: aead}
	records, err := db.SecretRecords(ctx)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		value, err := service.decrypt(record.Name, record.ValueBlob)
		if err != nil {
			return nil, fmt.Errorf("decrypt secret %q: %w", record.Name, err)
		}
		var allow []secretguard.AllowRule
		if err := json.Unmarshal([]byte(record.AllowJSON), &allow); err != nil {
			return nil, fmt.Errorf("decode secret %q rules: %w", record.Name, err)
		}
		spec := secretguard.SecretSpec{Name: record.Name, Value: string(value), Allow: allow, Action: secretguard.Action(record.Action)}
		if err := guard.Register(spec); err != nil {
			return nil, fmt.Errorf("load secret %q: %w", record.Name, err)
		}
		service.rules[record.Name] = Summary{Name: record.Name, Allow: cloneAllow(allow), Action: spec.Action}
	}
	return service, nil
}

func ParseMasterKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, errors.New("FORKGATE_MASTER_KEY is required")
	}
	if key, err := hex.DecodeString(raw); err == nil && len(key) == 32 {
		return key, nil
	}
	if key, err := base64.StdEncoding.DecodeString(raw); err == nil && len(key) == 32 {
		return key, nil
	}
	if key, err := base64.RawStdEncoding.DecodeString(raw); err == nil && len(key) == 32 {
		return key, nil
	}
	return nil, errors.New("FORKGATE_MASTER_KEY must encode exactly 32 bytes")
}

func (s *Service) Register(ctx context.Context, spec secretguard.SecretSpec) error {
	if spec.Action == "" {
		spec.Action = secretguard.ActionBlock
	}
	allowJSON, err := json.Marshal(spec.Allow)
	if err != nil {
		return fmt.Errorf("encode secret rules: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.rules[spec.Name]; exists {
		return fmt.Errorf("secret %q already exists", spec.Name)
	}
	if err := s.guard.Register(spec); err != nil {
		return err
	}
	ciphertext, err := s.encrypt(spec.Name, []byte(spec.Value))
	if err != nil {
		s.guard.Remove(spec.Name)
		return err
	}
	if err := s.store.SaveSecret(ctx, spec.Name, ciphertext, string(allowJSON), string(spec.Action)); err != nil {
		s.guard.Remove(spec.Name)
		return err
	}
	s.rules[spec.Name] = Summary{Name: spec.Name, Allow: cloneAllow(spec.Allow), Action: spec.Action}
	return nil
}

func (s *Service) List(_ context.Context) []Summary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Summary, 0, len(s.rules))
	for _, summary := range s.rules {
		summary.Allow = cloneAllow(summary.Allow)
		result = append(result, summary)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (s *Service) Delete(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.rules[name]; !exists {
		return nil
	}
	if err := s.store.DeleteSecret(ctx, name); err != nil {
		return err
	}
	s.guard.Remove(name)
	delete(s.rules, name)
	return nil
}

func (s *Service) encrypt(name string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate secret nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, []byte(name)), nil
}

func (s *Service) decrypt(name string, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < s.aead.NonceSize() {
		return nil, errors.New("secret ciphertext is truncated")
	}
	nonce, data := ciphertext[:s.aead.NonceSize()], ciphertext[s.aead.NonceSize():]
	return s.aead.Open(nil, nonce, data, []byte(name))
}

func cloneAllow(allow []secretguard.AllowRule) []secretguard.AllowRule {
	result := make([]secretguard.AllowRule, len(allow))
	for i, rule := range allow {
		result[i] = secretguard.AllowRule{Host: rule.Host, Locations: append([]string(nil), rule.Locations...)}
	}
	return result
}
