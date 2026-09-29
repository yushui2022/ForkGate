package branches

import (
	"context"
	"errors"
	"testing"

	"github.com/yushui2022/ForkGate/internal/store"
)

func newManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/branches.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s), s
}

func TestCreateAuthenticateAndFork(t *testing.T) {
	ctx := context.Background()
	m, s := newManager(t)
	created, err := m.CreateTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if created.Root.Mode != ModeLive || created.Root.Status != StatusActive {
		t.Fatalf("root = %+v", created.Root)
	}
	if got, err := m.Authenticate(ctx, created.RootToken); err != nil || got.ID != created.Root.ID {
		t.Fatalf("authenticate root = %+v, %v", got, err)
	}
	if created.RootToken == created.Root.TokenHash {
		t.Fatal("store record must not contain the raw token")
	}

	forked, err := m.RegisterFork(ctx, created.Root.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(forked.Children) != 3 {
		t.Fatalf("children = %+v", forked.Children)
	}
	if _, err := m.Authenticate(ctx, created.RootToken); !errors.Is(err, ErrBranchSealed) {
		t.Fatalf("old token error = %v, want ErrBranchSealed", err)
	}
	for _, child := range forked.Children {
		branch, err := m.Authenticate(ctx, child.Token)
		if err != nil {
			t.Fatalf("child authenticate: %v", err)
		}
		if branch.Mode != ModeSpeculative || branch.ParentID != created.Root.ID {
			t.Fatalf("child branch = %+v", branch)
		}
	}
	if _, err := m.RegisterFork(ctx, created.Root.ID, 1); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("sealed fork error = %v", err)
	}

	events, err := s.Events(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if stringValue(event.Payload["token"]) == created.RootToken {
			t.Fatal("raw root token was persisted in an event")
		}
	}
}

func TestAbortAndTerminalAuthentication(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t)
	created, err := m.CreateTree(ctx)
	if err != nil {
		t.Fatal(err)
	}
	forked, err := m.RegisterFork(ctx, created.Root.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	child := forked.Children[0]
	if err := m.Abort(ctx, child.BranchID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Authenticate(ctx, child.Token); !errors.Is(err, ErrBranchTerminal) {
		t.Fatalf("aborted token error = %v", err)
	}
	if err := m.Abort(ctx, child.BranchID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("second abort error = %v", err)
	}
	if _, err := m.RegisterFork(ctx, child.BranchID, 1); !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrNestedFork) {
		t.Fatalf("aborted fork error = %v", err)
	}
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}
