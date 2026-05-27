package sqlstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
	_ "modernc.org/sqlite"
)

func TestPurgeTerminalUsesLastMutationTime(t *testing.T) {
	ctx := context.Background()
	adapter := openTestAdapter(ctx, t)
	old := time.Now().Add(-5 * time.Hour)

	stale, err := adapter.Create(ctx, coordstore.Record{
		ID:        "stale-terminal",
		Title:     "stale terminal",
		Status:    "closed",
		Type:      "task",
		CreatedAt: old,
	})
	if err != nil {
		t.Fatalf("create stale terminal: %v", err)
	}
	recentlyClosed, err := adapter.Create(ctx, coordstore.Record{
		ID:        "recently-closed",
		Title:     "recently closed",
		Status:    "open",
		Type:      "task",
		CreatedAt: old,
	})
	if err != nil {
		t.Fatalf("create recently closed: %v", err)
	}
	if err := adapter.Update(ctx, recentlyClosed.ID, coordstore.Update{Status: "closed"}); err != nil {
		t.Fatalf("close recently-closed: %v", err)
	}

	purged, err := adapter.PurgeTerminal(ctx, 4*time.Hour)
	if err != nil {
		t.Fatalf("PurgeTerminal: %v", err)
	}
	if purged != 1 {
		t.Fatalf("PurgeTerminal purged %d records, want 1", purged)
	}
	if _, err := adapter.Get(ctx, stale.ID); !coordstore.IsNotFound(err) {
		t.Fatalf("Get(%q) error = %v, want not found", stale.ID, err)
	}
	if _, err := adapter.Get(ctx, recentlyClosed.ID); err != nil {
		t.Fatalf("Get(%q): %v", recentlyClosed.ID, err)
	}
}

func TestPrimeScanExcludesSingleLCanceled(t *testing.T) {
	ctx := context.Background()
	adapter := openTestAdapter(ctx, t)
	if _, err := adapter.Create(ctx, coordstore.Record{
		ID:     "open-record",
		Title:  "open record",
		Status: "open",
		Type:   "task",
	}); err != nil {
		t.Fatalf("create open: %v", err)
	}
	if _, err := adapter.Create(ctx, coordstore.Record{
		ID:     "canceled-record",
		Title:  "canceled record",
		Status: "canceled",
		Type:   "task",
	}); err != nil {
		t.Fatalf("create canceled: %v", err)
	}

	count, err := adapter.PrimeScan(ctx)
	if err != nil {
		t.Fatalf("PrimeScan: %v", err)
	}
	if count != 1 {
		t.Fatalf("PrimeScan count = %d, want 1", count)
	}
}

func openTestAdapter(ctx context.Context, t *testing.T) *Adapter {
	t.Helper()
	adapter := New(sqliteTestDialect, filepath.Join(t.TempDir(), "store.db"), "ts")
	if err := adapter.Open(ctx, coordstore.Config{}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := adapter.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	return adapter
}

var sqliteTestDialect = Dialect{
	Name:        "sqlstore-test-sqlite",
	Driver:      "sqlite",
	Placeholder: func(int) string { return "?" },
	Schema: []string{
		`CREATE TABLE IF NOT EXISTS records (
		    id TEXT PRIMARY KEY,
		    title TEXT NOT NULL DEFAULT '',
		    status TEXT NOT NULL DEFAULT 'open',
		    type TEXT NOT NULL DEFAULT 'task',
		    priority INTEGER NOT NULL DEFAULT 0,
		    created_at INTEGER NOT NULL,
		    updated_at INTEGER NOT NULL DEFAULT 0,
		    assignee TEXT NOT NULL DEFAULT '',
		    parent_id TEXT NOT NULL DEFAULT '',
		    description TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS ephemeral (
		    id TEXT PRIMARY KEY,
		    title TEXT NOT NULL DEFAULT '',
		    status TEXT NOT NULL DEFAULT 'open',
		    type TEXT NOT NULL DEFAULT 'message',
		    created_at INTEGER NOT NULL,
		    updated_at INTEGER NOT NULL DEFAULT 0,
		    assignee TEXT NOT NULL DEFAULT '',
		    parent_id TEXT NOT NULL DEFAULT '',
		    expires_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS labels (
		    record_id TEXT NOT NULL,
		    label TEXT NOT NULL,
		    PRIMARY KEY(record_id, label)
		)`,
		`CREATE TABLE IF NOT EXISTS metadata (
		    record_id TEXT NOT NULL,
		    meta_key TEXT NOT NULL,
		    meta_value TEXT NOT NULL,
		    PRIMARY KEY(record_id, meta_key)
		)`,
		`CREATE TABLE IF NOT EXISTS ephemeral_labels (
		    record_id TEXT NOT NULL,
		    label TEXT NOT NULL,
		    PRIMARY KEY(record_id, label)
		)`,
		`CREATE TABLE IF NOT EXISTS ephemeral_metadata (
		    record_id TEXT NOT NULL,
		    meta_key TEXT NOT NULL,
		    meta_value TEXT NOT NULL,
		    PRIMARY KEY(record_id, meta_key)
		)`,
		`CREATE TABLE IF NOT EXISTS deps (
		    issue_id TEXT NOT NULL,
		    depends_on_id TEXT NOT NULL,
		    dep_type TEXT NOT NULL DEFAULT 'blocks',
		    PRIMARY KEY(issue_id, depends_on_id)
		)`,
	},
	InsertLabel:      `INSERT INTO {{table}}(record_id,label) VALUES(?,?) ON CONFLICT DO NOTHING`,
	InsertMetadata:   `INSERT INTO {{table}}(record_id,meta_key,meta_value) VALUES(?,?,?) ON CONFLICT DO NOTHING`,
	UpsertMetadata:   `INSERT INTO {{table}}(record_id,meta_key,meta_value) VALUES(?,?,?) ON CONFLICT(record_id,meta_key) DO UPDATE SET meta_value=excluded.meta_value`,
	UpsertDependency: `INSERT INTO deps(issue_id,depends_on_id,dep_type) VALUES(?,?,?) ON CONFLICT(issue_id,depends_on_id) DO UPDATE SET dep_type=excluded.dep_type`,
}
