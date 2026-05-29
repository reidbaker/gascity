package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCopyBeadsIntoCoordstoreDryRunCountsDepsWithoutTarget(t *testing.T) {
	created := time.Unix(100, 0).UTC()
	source := beads.NewMemStoreFrom(2, []beads.Bead{
		{ID: "ga-1", Title: "blocker", Status: "open", Type: "task", CreatedAt: created, UpdatedAt: created},
		{ID: "ga-2", Title: "work", Status: "open", Type: "task", CreatedAt: created.Add(time.Second), UpdatedAt: created.Add(time.Second)},
	}, []beads.Dep{
		{IssueID: "ga-2", DependsOnID: "ga-1", Type: "blocks"},
		{IssueID: "ga-2", DependsOnID: "ga-missing", Type: "blocks"},
	})

	summary, err := copyBeadsIntoCoordstore(source, nil, true)
	if err != nil {
		t.Fatalf("copyBeadsIntoCoordstore dry run: %v", err)
	}
	if summary.SourceCount != 2 || summary.Deps != 1 || summary.Imported != 0 || summary.Skipped != 0 || !summary.DryRun {
		t.Fatalf("dry-run summary = %+v, want source=2 importable deps=1 no writes", summary)
	}
}

func TestDiffCoordstoreShadowDetectsDependencyMismatch(t *testing.T) {
	created := time.Unix(100, 0).UTC()
	sourceBeads := []beads.Bead{
		{ID: "ga-1", Title: "blocker", Status: "open", Type: "task", CreatedAt: created, UpdatedAt: created},
		{ID: "ga-2", Title: "work", Status: "open", Type: "task", CreatedAt: created.Add(time.Second), UpdatedAt: created.Add(time.Second)},
	}
	targetBeads := []beads.Bead{
		{ID: "ga-1", Title: "blocker", Status: "open", Type: "task", CreatedAt: created, UpdatedAt: created},
		{ID: "ga-2", Title: "work", Status: "open", Type: "task", CreatedAt: created.Add(time.Second), UpdatedAt: created.Add(time.Second)},
	}
	source := beads.NewMemStoreFrom(2, sourceBeads, []beads.Dep{
		{IssueID: "ga-2", DependsOnID: "ga-1", Type: "blocks"},
	})
	target := beads.NewMemStoreFrom(2, targetBeads, nil)

	summary, err := diffCoordstoreShadow(source, target)
	if err != nil {
		t.Fatalf("diffCoordstoreShadow: %v", err)
	}
	if summary.OK {
		t.Fatal("shadow summary OK = true, want dependency mismatch")
	}
	if !reflect.DeepEqual(summary.Corrupted, []string{"ga-2"}) {
		t.Fatalf("corrupted = %+v, want [ga-2]", summary.Corrupted)
	}
}
