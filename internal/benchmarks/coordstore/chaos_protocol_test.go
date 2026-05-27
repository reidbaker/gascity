package coordstore

import (
	"context"
	"testing"
	"time"
)

func TestRecordContentFingerprintDetectsCorruption(t *testing.T) {
	base := Record{
		ID:        "rec-1",
		Title:     "expected",
		Status:    "open",
		Type:      "task",
		Priority:  2,
		CreatedAt: time.Unix(10, 20),
		UpdatedAt: time.Unix(11, 21),
		Assignee:  "builder",
		Labels:    []string{"b", "a"},
		Metadata:  map[string]string{"z": "last", "a": "first"},
	}
	same := base
	same.Labels = []string{"a", "b"}
	same.Metadata = map[string]string{"a": "first", "z": "last"}
	corrupted := base
	corrupted.Title = "changed"

	baseFingerprint, err := recordContentFingerprint(base)
	if err != nil {
		t.Fatalf("base fingerprint: %v", err)
	}
	sameFingerprint, err := recordContentFingerprint(same)
	if err != nil {
		t.Fatalf("same fingerprint: %v", err)
	}
	corruptedFingerprint, err := recordContentFingerprint(corrupted)
	if err != nil {
		t.Fatalf("corrupted fingerprint: %v", err)
	}

	if baseFingerprint != sameFingerprint {
		t.Fatalf("fingerprint changed for equivalent content: %q != %q", baseFingerprint, sameFingerprint)
	}
	if baseFingerprint == corruptedFingerprint {
		t.Fatalf("fingerprint did not change for corrupted content: %q", baseFingerprint)
	}
}

func TestCheckAckedRecordContentReportsMissingAndCorrupted(t *testing.T) {
	ctx := context.Background()
	expectedRecord := Record{ID: "ok", Title: "expected", Status: "open", Type: "task"}
	expectedFingerprint, err := recordContentFingerprint(expectedRecord)
	if err != nil {
		t.Fatalf("expected fingerprint: %v", err)
	}
	corruptedRecord := Record{ID: "corrupt", Title: "expected", Status: "open", Type: "task"}
	corruptedExpected, err := recordContentFingerprint(corruptedRecord)
	if err != nil {
		t.Fatalf("corrupted expected fingerprint: %v", err)
	}
	store := recordGetterFunc(func(_ context.Context, id string) (Record, error) {
		switch id {
		case "ok":
			return expectedRecord, nil
		case "corrupt":
			return Record{ID: "corrupt", Title: "changed", Status: "open", Type: "task"}, nil
		default:
			return Record{}, ErrNotFound
		}
	})

	result, err := checkAckedRecordContent(ctx, store, []string{"ok", "corrupt", "missing"}, map[string]string{
		"ok":      expectedFingerprint,
		"corrupt": corruptedExpected,
		"missing": "expected",
	})
	if err != nil {
		t.Fatalf("check content: %v", err)
	}
	if len(result.Found) != 2 {
		t.Fatalf("found = %d, want 2", len(result.Found))
	}
	if len(result.Missing) != 1 || result.Missing[0] != "missing" {
		t.Fatalf("missing = %v, want [missing]", result.Missing)
	}
	if len(result.Corrupted) != 1 || result.Corrupted[0] != "corrupt" {
		t.Fatalf("corrupted = %v, want [corrupt]", result.Corrupted)
	}
}

type recordGetterFunc func(context.Context, string) (Record, error)

func (f recordGetterFunc) Get(ctx context.Context, id string) (Record, error) {
	return f(ctx, id)
}
