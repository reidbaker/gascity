package beads

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

type hqWALOp string

const (
	hqWALCreate      hqWALOp = "create"
	hqWALUpsert      hqWALOp = "upsert"
	hqWALUpsertBatch hqWALOp = "upsert_batch"
	hqWALDelete      hqWALOp = "delete"
	hqWALDeleteBatch hqWALOp = "delete_batch"
	hqWALDepAdd      hqWALOp = "dep_add"
	hqWALDepRemove   hqWALOp = "dep_remove"
)

type hqWALEntry struct {
	Op       hqWALOp `json:"op"`
	ID       string  `json:"id,omitempty"`
	TargetID string  `json:"target_id,omitempty"`
	Bead     *Bead   `json:"bead,omitempty"`
	Beads    []Bead  `json:"beads,omitempty"`
	Dep      *Dep    `json:"dep,omitempty"`
}

type hqWAL struct {
	path       string
	file       *os.File
	syncWrites bool
}

func openHQWAL(path string, syncWrites bool) (*hqWAL, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening hqstore wal: %w", err)
	}
	return &hqWAL{path: path, file: file, syncWrites: syncWrites}, nil
}

func (w *hqWAL) append(entry hqWALEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encoding hqstore wal entry: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.file.Write(data); err != nil {
		return fmt.Errorf("writing hqstore wal: %w", err)
	}
	if w.syncWrites {
		if err := w.file.Sync(); err != nil {
			return fmt.Errorf("syncing hqstore wal: %w", err)
		}
	}
	return nil
}

func (w *hqWAL) truncate() error {
	if err := w.file.Truncate(0); err != nil {
		return fmt.Errorf("truncating hqstore wal: %w", err)
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking hqstore wal: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("syncing truncated hqstore wal: %w", err)
	}
	return nil
}

func (w *hqWAL) close() error {
	if w.file == nil {
		return nil
	}
	if err := w.file.Sync(); err != nil {
		return err
	}
	if err := w.file.Close(); err != nil {
		return err
	}
	w.file = nil
	return nil
}

func (s *HQStore) replayWAL() error {
	path := s.walPath()
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("opening hqstore wal for replay: %w", err)
	}
	defer file.Close() //nolint:errcheck

	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var entry hqWALEntry
			if decodeErr := json.Unmarshal(line, &entry); decodeErr != nil {
				return fmt.Errorf("replaying hqstore wal: %w", decodeErr)
			}
			if applyErr := s.applyWALEntryLocked(entry); applyErr != nil {
				return applyErr
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("reading hqstore wal: %w", err)
	}
}

func (s *HQStore) applyWALEntryLocked(entry hqWALEntry) error {
	switch entry.Op {
	case hqWALCreate, hqWALUpsert:
		if entry.Bead == nil {
			return fmt.Errorf("replaying hqstore wal: %s missing bead", entry.Op)
		}
		s.upsertLocked(*entry.Bead)
		if entry.Op == hqWALCreate {
			for _, dep := range depsFromNeeds(*entry.Bead) {
				s.depAddCoreLocked(dep.IssueID, dep.DependsOnID, dep.Type)
			}
		}
	case hqWALUpsertBatch:
		for _, bead := range entry.Beads {
			s.upsertLocked(bead)
		}
	case hqWALDelete:
		s.deleteLocked(entry.ID)
	case hqWALDeleteBatch:
		for _, bead := range entry.Beads {
			s.deleteLocked(bead.ID)
		}
	case hqWALDepAdd:
		if entry.Dep == nil {
			return fmt.Errorf("replaying hqstore wal: dep_add missing dep")
		}
		s.depAddCoreLocked(entry.Dep.IssueID, entry.Dep.DependsOnID, entry.Dep.Type)
	case hqWALDepRemove:
		s.depRemoveCoreLocked(entry.ID, entry.TargetID)
	default:
		return fmt.Errorf("replaying hqstore wal: unknown op %q", entry.Op)
	}
	return nil
}

func (s *HQStore) walPath() string {
	return s.dir + string(os.PathSeparator) + hqWALFileName
}
