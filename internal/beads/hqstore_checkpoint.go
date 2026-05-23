package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type hqCheckpoint struct {
	Seq   int      `json:"seq"`
	Main  []Bead   `json:"main,omitempty"`
	Wisps []Bead   `json:"wisps,omitempty"`
	Deps  []Dep    `json:"deps,omitempty"`
	Order []string `json:"order,omitempty"`
}

// Checkpoint writes an atomic snapshot of the current HQStore state and clears
// the WAL entries already represented by that snapshot.
func (s *HQStore) Checkpoint() error {
	s.lockWriter()
	defer s.unlockWriter()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpenLocked(); err != nil {
		return err
	}
	return s.checkpointLocked()
}

func (s *HQStore) checkpointLocked() error {
	cp := hqCheckpoint{
		Seq:   s.seq,
		Main:  make([]Bead, 0, len(s.main)),
		Wisps: make([]Bead, 0, len(s.wisps)),
		Deps:  snapshotHQDeps(s.deps),
		Order: slicesCloneString(s.order),
	}
	for _, id := range s.order {
		if b, ok := s.main[id]; ok {
			cp.Main = append(cp.Main, cloneBead(b))
		}
		if b, ok := s.wisps[id]; ok {
			cp.Wisps = append(cp.Wisps, cloneBead(b))
		}
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding hqstore checkpoint: %w", err)
	}
	data = append(data, '\n')

	path := filepath.Join(s.dir, hqCheckpointFileName)
	tmp, err := os.CreateTemp(s.dir, ".checkpoint-*.tmp")
	if err != nil {
		return fmt.Errorf("creating hqstore checkpoint temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing hqstore checkpoint: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("syncing hqstore checkpoint: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing hqstore checkpoint: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("publishing hqstore checkpoint: %w", err)
	}
	if s.wal != nil {
		if err := s.wal.truncate(); err != nil {
			return err
		}
	}
	s.writesSinceCP = 0
	return nil
}

func (s *HQStore) loadCheckpoint() error {
	data, err := os.ReadFile(filepath.Join(s.dir, hqCheckpointFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading hqstore checkpoint: %w", err)
	}
	var cp hqCheckpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return fmt.Errorf("decoding hqstore checkpoint: %w", err)
	}
	s.resetCoreLocked()
	s.seq = cp.Seq

	if len(cp.Order) == 0 {
		for _, b := range cp.Main {
			cp.Order = append(cp.Order, b.ID)
		}
		for _, b := range cp.Wisps {
			cp.Order = append(cp.Order, b.ID)
		}
	}
	byID := make(map[string]Bead, len(cp.Main)+len(cp.Wisps))
	for _, b := range cp.Main {
		b.Ephemeral = false
		byID[b.ID] = b
	}
	for _, b := range cp.Wisps {
		b.Ephemeral = true
		byID[b.ID] = b
	}
	for _, id := range cp.Order {
		b, ok := byID[id]
		if !ok {
			continue
		}
		s.upsertLocked(b)
		delete(byID, id)
	}
	for _, b := range byID {
		s.upsertLocked(b)
	}
	s.deps = snapshotHQDeps(cp.Deps)
	return nil
}

func slicesCloneString(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}
