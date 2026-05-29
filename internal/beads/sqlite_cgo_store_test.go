//go:build cgo && sqlite_cgo

package beads

import (
	"errors"
	"testing"
	"time"
)

func TestSQLiteCGOStorePersistsQueriesAndReady(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteCGOStore(dir, WithSQLiteCGOStoreIDPrefix("ga"), WithSQLiteCGOStoreRetention(0, 0))
	if err != nil {
		t.Fatalf("OpenSQLiteCGOStore: %v", err)
	}
	blocker, err := store.Create(Bead{Title: "blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	priority := 2
	work, err := store.Create(Bead{
		Title:    "work",
		Assignee: "agent",
		Priority: &priority,
		ParentID: "parent-1",
		Labels:   []string{"ready-to-build"},
		Metadata: map[string]string{"gc.routed_to": "agent"},
		Needs:    []string{blocker.ID},
	})
	if err != nil {
		t.Fatalf("Create work: %v", err)
	}
	if work.ID != "ga-2" {
		t.Fatalf("work ID = %q, want ga-2", work.ID)
	}

	byMetadata, err := store.ListByMetadata(map[string]string{"gc.routed_to": "agent"}, 0)
	if err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	if len(byMetadata) != 1 || byMetadata[0].ID != work.ID {
		t.Fatalf("ListByMetadata = %+v, want work bead", byMetadata)
	}
	ready, err := store.Ready(ReadyQuery{Assignee: "agent"})
	if err != nil {
		t.Fatalf("Ready blocked: %v", err)
	}
	if len(ready) != 0 {
		t.Fatalf("Ready while blocked = %+v, want none", ready)
	}
	if err := store.Close(blocker.ID); err != nil {
		t.Fatalf("Close blocker: %v", err)
	}
	ready, err = store.Ready(ReadyQuery{Assignee: "agent"})
	if err != nil {
		t.Fatalf("Ready unblocked: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != work.ID {
		t.Fatalf("Ready after close = %+v, want work bead", ready)
	}

	reopened, err := OpenSQLiteCGOStore(dir, WithSQLiteCGOStoreIDPrefix("ga"), WithSQLiteCGOStoreRetention(0, 0))
	if err != nil {
		t.Fatalf("reopen SQLiteCGOStore: %v", err)
	}
	got, err := reopened.Get(work.ID)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Metadata["gc.routed_to"] != "agent" || got.ParentID != "parent-1" || got.Priority == nil || *got.Priority != priority {
		t.Fatalf("reopened bead = %+v, want persisted fields", got)
	}
	next, err := reopened.Create(Bead{Title: "next"})
	if err != nil {
		t.Fatalf("Create after reopen: %v", err)
	}
	if next.ID != "ga-3" {
		t.Fatalf("next ID = %q, want ga-3", next.ID)
	}
}

func TestSQLiteCGOStorePreservesImportedIDAndTimestamp(t *testing.T) {
	created := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	updated := created.Add(10 * time.Minute)
	store, err := OpenSQLiteCGOStore(t.TempDir(), WithSQLiteCGOStoreIDPrefix("ga"), WithSQLiteCGOStoreRetention(0, 0))
	if err != nil {
		t.Fatalf("OpenSQLiteCGOStore: %v", err)
	}
	imported, err := store.Create(Bead{
		ID:        "ga-99",
		Title:     "imported",
		Status:    "closed",
		CreatedAt: created,
		UpdatedAt: updated,
	})
	if err != nil {
		t.Fatalf("Create imported: %v", err)
	}
	if imported.ID != "ga-99" || !imported.UpdatedAt.Equal(updated) || imported.Status != "closed" {
		t.Fatalf("imported = %+v, want preserved ID/status/timestamps", imported)
	}
	next, err := store.Create(Bead{Title: "next"})
	if err != nil {
		t.Fatalf("Create next: %v", err)
	}
	if next.ID != "ga-100" {
		t.Fatalf("next ID = %q, want ga-100", next.ID)
	}
	if _, err := store.Create(Bead{ID: "ga-99", Title: "duplicate"}); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Create duplicate err = %v, want duplicate error", err)
	}
}
