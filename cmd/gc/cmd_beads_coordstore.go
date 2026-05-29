package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

func newBeadsCoordstoreCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "coordstore",
		Short:  "Manage the opt-in SQLite-CGo coordstore backend",
		Hidden: true,
	}
	cmd.AddCommand(
		newBeadsCoordstoreImportCmd(stdout, stderr),
		newBeadsCoordstoreShadowCmd(stdout, stderr),
	)
	return cmd
}

func newBeadsCoordstoreImportCmd(stdout, stderr io.Writer) *cobra.Command {
	var cityPath string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import city beads from Dolt/bd into SQLite-CGo coordstore",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doBeadsCoordstoreImport(cityPath, dryRun, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cityPath, "city", "", "city root (default: resolve from cwd)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "count records without writing coordstore")
	return cmd
}

func newBeadsCoordstoreShadowCmd(stdout, stderr io.Writer) *cobra.Command {
	var cityPath string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "shadow",
		Short: "Diff Dolt/bd against SQLite-CGo coordstore",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doBeadsCoordstoreShadow(cityPath, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cityPath, "city", "", "city root (default: resolve from cwd)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON summary")
	return cmd
}

type coordstoreImportSummary struct {
	SourceCount int  `json:"source_count"`
	Imported    int  `json:"imported"`
	Skipped     int  `json:"skipped"`
	Deps        int  `json:"deps"`
	DryRun      bool `json:"dry_run"`
}

type coordstoreShadowSummary struct {
	SourceCount int      `json:"source_count"`
	TargetCount int      `json:"target_count"`
	Missing     []string `json:"missing,omitempty"`
	Extra       []string `json:"extra,omitempty"`
	Corrupted   []string `json:"corrupted,omitempty"`
	OK          bool     `json:"ok"`
}

func doBeadsCoordstoreImport(cityFlag string, dryRun bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCoordstoreCity(cityFlag)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore import: %v\n", err) //nolint:errcheck
		return 1
	}
	src, err := openBdStoreAt(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore import: open bd source: %v\n", err) //nolint:errcheck
		return 1
	}
	var dst beads.Store
	if !dryRun {
		dst, err = openCoordStoreAt(cityPath, cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc beads coordstore import: open coordstore target: %v\n", err) //nolint:errcheck
			return 1
		}
	}
	summary, err := copyBeadsIntoCoordstore(src, dst, dryRun)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore import: %v\n", err) //nolint:errcheck
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "coordstore import: source=%d imported=%d skipped=%d deps=%d dry_run=%t\n",
		summary.SourceCount, summary.Imported, summary.Skipped, summary.Deps, summary.DryRun)
	return 0
}

func doBeadsCoordstoreShadow(cityFlag string, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCoordstoreCity(cityFlag)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore shadow: %v\n", err) //nolint:errcheck
		return 1
	}
	src, err := openBdStoreAt(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore shadow: open bd source: %v\n", err) //nolint:errcheck
		return 1
	}
	dst, err := openCoordStoreAt(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore shadow: open coordstore target: %v\n", err) //nolint:errcheck
		return 1
	}
	summary, err := diffCoordstoreShadow(src, dst)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads coordstore shadow: %v\n", err) //nolint:errcheck
		return 1
	}
	if jsonOut {
		if err := writeCLIJSONLine(stdout, summary); err != nil {
			fmt.Fprintf(stderr, "gc beads coordstore shadow: %v\n", err) //nolint:errcheck
			return 1
		}
	} else {
		_, _ = fmt.Fprintf(stdout, "coordstore shadow: source=%d target=%d missing=%d extra=%d corrupted=%d ok=%t\n",
			summary.SourceCount, summary.TargetCount, len(summary.Missing), len(summary.Extra), len(summary.Corrupted), summary.OK)
	}
	if !summary.OK {
		return 1
	}
	return 0
}

func resolveCoordstoreCity(cityFlag string) (string, error) {
	if strings.TrimSpace(cityFlag) != "" {
		return filepath.Abs(filepath.Clean(cityFlag))
	}
	return resolveCity()
}

func copyBeadsIntoCoordstore(src, dst beads.Store, dryRun bool) (coordstoreImportSummary, error) {
	source, err := src.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth, Sort: beads.SortCreatedAsc})
	if err != nil {
		return coordstoreImportSummary{}, fmt.Errorf("list source beads: %w", err)
	}
	sourceIDs := make(map[string]bool, len(source))
	for _, b := range source {
		sourceIDs[b.ID] = true
	}
	summary := coordstoreImportSummary{SourceCount: len(source), DryRun: dryRun}
	for _, b := range source {
		if !dryRun {
			if dst == nil {
				return summary, fmt.Errorf("coordstore target is required")
			}
			if _, err := dst.Get(b.ID); err == nil {
				summary.Skipped++
			} else if !errors.Is(err, beads.ErrNotFound) {
				return summary, fmt.Errorf("probe target bead %q: %w", b.ID, err)
			} else {
				if _, err := dst.Create(b); err != nil {
					return summary, fmt.Errorf("import bead %q: %w", b.ID, err)
				}
				summary.Imported++
			}
		}
		deps, err := src.DepList(b.ID, "down")
		if err != nil {
			return summary, fmt.Errorf("list deps for %q: %w", b.ID, err)
		}
		for _, dep := range deps {
			if dep.Type == "" {
				dep.Type = "blocks"
			}
			if !sourceIDs[dep.DependsOnID] {
				continue
			}
			if !dryRun {
				if err := dst.DepAdd(dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
					return summary, fmt.Errorf("import dep %s -> %s: %w", dep.IssueID, dep.DependsOnID, err)
				}
			}
			summary.Deps++
		}
	}
	return summary, nil
}

func diffCoordstoreShadow(src, dst beads.Store) (coordstoreShadowSummary, error) {
	source, err := src.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		return coordstoreShadowSummary{}, fmt.Errorf("list source beads: %w", err)
	}
	target, err := dst.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		return coordstoreShadowSummary{}, fmt.Errorf("list target beads: %w", err)
	}
	sourceByID := make(map[string]beads.Bead, len(source))
	for _, b := range source {
		sourceByID[b.ID] = b
	}
	sourceIDs := make(map[string]bool, len(sourceByID))
	for id := range sourceByID {
		sourceIDs[id] = true
	}
	targetByID := make(map[string]beads.Bead, len(target))
	for _, b := range target {
		targetByID[b.ID] = b
	}
	summary := coordstoreShadowSummary{SourceCount: len(source), TargetCount: len(target)}
	corrupted := make(map[string]bool)
	for id, sbead := range sourceByID {
		tbead, ok := targetByID[id]
		if !ok {
			summary.Missing = append(summary.Missing, id)
			continue
		}
		if coordstoreBeadFingerprint(sbead) != coordstoreBeadFingerprint(tbead) {
			corrupted[id] = true
		}
		srcDeps, err := coordstoreDepFingerprint(src, id, sourceIDs)
		if err != nil {
			return summary, err
		}
		dstDeps, err := coordstoreDepFingerprint(dst, id, sourceIDs)
		if err != nil {
			return summary, err
		}
		if srcDeps != dstDeps {
			corrupted[id] = true
		}
	}
	for id := range targetByID {
		if _, ok := sourceByID[id]; !ok {
			summary.Extra = append(summary.Extra, id)
		}
	}
	for id := range corrupted {
		summary.Corrupted = append(summary.Corrupted, id)
	}
	sort.Strings(summary.Missing)
	sort.Strings(summary.Extra)
	sort.Strings(summary.Corrupted)
	summary.OK = len(summary.Missing) == 0 && len(summary.Extra) == 0 && len(summary.Corrupted) == 0
	return summary, nil
}

func coordstoreDepFingerprint(store beads.Store, id string, validIDs map[string]bool) (string, error) {
	deps, err := store.DepList(id, "down")
	if err != nil {
		return "", fmt.Errorf("list deps for %q: %w", id, err)
	}
	normalized := deps[:0]
	for _, dep := range deps {
		if validIDs != nil && (!validIDs[dep.IssueID] || !validIDs[dep.DependsOnID]) {
			continue
		}
		if dep.Type == "" {
			dep.Type = "blocks"
		}
		normalized = append(normalized, dep)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].IssueID != normalized[j].IssueID {
			return normalized[i].IssueID < normalized[j].IssueID
		}
		if normalized[i].DependsOnID != normalized[j].DependsOnID {
			return normalized[i].DependsOnID < normalized[j].DependsOnID
		}
		return normalized[i].Type < normalized[j].Type
	})
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(normalized)
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func coordstoreBeadFingerprint(b beads.Bead) string {
	type stableBead struct {
		ID          string            `json:"id"`
		Title       string            `json:"title"`
		Status      string            `json:"status"`
		Type        string            `json:"type"`
		Priority    *int              `json:"priority,omitempty"`
		CreatedAt   time.Time         `json:"created_at"`
		UpdatedAt   time.Time         `json:"updated_at,omitempty"`
		Assignee    string            `json:"assignee,omitempty"`
		From        string            `json:"from,omitempty"`
		ParentID    string            `json:"parent,omitempty"`
		Ref         string            `json:"ref,omitempty"`
		Needs       []string          `json:"needs,omitempty"`
		Description string            `json:"description,omitempty"`
		Labels      []string          `json:"labels,omitempty"`
		Metadata    map[string]string `json:"metadata,omitempty"`
		Ephemeral   bool              `json:"ephemeral,omitempty"`
	}
	stable := stableBead{
		ID:          b.ID,
		Title:       b.Title,
		Status:      b.Status,
		Type:        b.Type,
		Priority:    cloneIntPtrForCoordstore(b.Priority),
		CreatedAt:   b.CreatedAt,
		UpdatedAt:   b.UpdatedAt,
		Assignee:    b.Assignee,
		From:        b.From,
		ParentID:    b.ParentID,
		Ref:         b.Ref,
		Needs:       append([]string(nil), b.Needs...),
		Description: b.Description,
		Labels:      append([]string(nil), b.Labels...),
		Metadata:    maps.Clone(b.Metadata),
		Ephemeral:   b.Ephemeral,
	}
	sort.Strings(stable.Needs)
	sort.Strings(stable.Labels)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(stable)
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:])
}

func cloneIntPtrForCoordstore(v *int) *int {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}
