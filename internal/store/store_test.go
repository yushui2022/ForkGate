package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRecordEventAndCount(t *testing.T) {
	s, err := Open(t.TempDir() + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecordEvent(context.Background(), "tree-1", "branch-1", "request.forwarded", map[string]any{"status": 200}); err != nil {
		t.Fatal(err)
	}
	count, err := s.EventCount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("event count = %d, want 1", count)
	}
}

func TestEventsCursorSurvivesRestartAndSpecialPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events?#.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEvent(context.Background(), "tree-1", "branch-1", "first", map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEvent(context.Background(), "tree-1", "branch-2", "second", map[string]any{"n": 2}); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(context.Background(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ID != 1 || events[0].Type != "first" {
		t.Fatalf("first page = %+v", events)
	}
	cursor := events[0].ID
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err = s.Events(context.Background(), cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ID != 2 || events[0].Payload["n"] != float64(2) {
		t.Fatalf("cursor page = %+v", events)
	}
}
