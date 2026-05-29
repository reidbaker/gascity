//go:build cgo && sqlite_cgo

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCoordstoreProviderOpensSQLiteStoreWithoutBdContract(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\nprefix = \"ga\"\n\n[beads]\nprovider = \"coordstore\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	if got := rawBeadsProvider(cityDir); got != "coordstore" {
		t.Fatalf("rawBeadsProvider = %q, want coordstore", got)
	}
	if cityUsesBdStoreContract(cityDir) {
		t.Fatal("cityUsesBdStoreContract = true, want false for coordstore")
	}
	if got := beadsProvider(cityDir); got != "coordstore" {
		t.Fatalf("beadsProvider = %q, want coordstore", got)
	}
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	created, err := store.Create(beads.Bead{Title: "coordstore bead"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != "ga-1" {
		t.Fatalf("created ID = %q, want ga-1", created.ID)
	}
	reopened, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if _, err := reopened.Get(created.ID); err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gc", "coordstore", "beads.sqlite")); err != nil {
		t.Fatalf("coordstore db stat: %v", err)
	}
}
