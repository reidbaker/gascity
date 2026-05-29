//go:build cgo && sqlite_cgo

package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	sqliteCGOStoreFilename               = "beads.sqlite"
	sqliteCGODefaultPrefix               = "gc"
	sqliteCGODefaultRetentionPeriod      = 4 * time.Hour
	sqliteCGODefaultRetentionSweepPeriod = 30 * time.Second
)

// SQLiteCGOStoreOptions configures the SQLite-CGo bead store.
type SQLiteCGOStoreOptions struct {
	prefix                  string
	retentionPeriod         time.Duration
	retentionSweepInterval  time.Duration
	disableRetentionSweeper bool
}

// SQLiteCGOStoreOption customizes OpenSQLiteCGOStore.
type SQLiteCGOStoreOption func(*SQLiteCGOStoreOptions)

// WithSQLiteCGOStoreIDPrefix sets the generated bead ID prefix.
func WithSQLiteCGOStoreIDPrefix(prefix string) SQLiteCGOStoreOption {
	return func(o *SQLiteCGOStoreOptions) {
		if strings.TrimSpace(prefix) != "" {
			o.prefix = normalizeIDPrefix(prefix)
		}
	}
}

// WithSQLiteCGOStoreRetention configures terminal-record retention. A
// non-positive sweep interval disables the background sweeper.
func WithSQLiteCGOStoreRetention(period, sweepInterval time.Duration) SQLiteCGOStoreOption {
	return func(o *SQLiteCGOStoreOptions) {
		o.retentionPeriod = period
		o.retentionSweepInterval = sweepInterval
		o.disableRetentionSweeper = sweepInterval <= 0
	}
}

// SQLiteCGOStore is a SQLite-backed Store implementation for the selected
// coordination-store backend. It uses mattn/go-sqlite3 behind the sqlite_cgo
// build tag so the default build remains unchanged.
type SQLiteCGOStore struct {
	db                      *sql.DB
	path                    string
	prefix                  string
	retentionPeriod         time.Duration
	retentionSweepInterval  time.Duration
	disableRetentionSweeper bool
	retentionStop           chan struct{}
}

// OpenSQLiteCGOStore opens or creates a SQLite-CGo bead store under dir.
func OpenSQLiteCGOStore(dir string, opts ...SQLiteCGOStoreOption) (Store, error) {
	cfg := SQLiteCGOStoreOptions{
		prefix:                 sqliteCGODefaultPrefix,
		retentionPeriod:        sqliteCGODefaultRetentionPeriod,
		retentionSweepInterval: sqliteCGODefaultRetentionSweepPeriod,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.prefix == "" {
		cfg.prefix = sqliteCGODefaultPrefix
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("opening sqlite-cgo store: %w", err)
	}
	dbPath := filepath.Join(dir, sqliteCGOStoreFilename)
	db, err := sql.Open("sqlite3", sqliteCGODSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("opening sqlite-cgo store %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	s := &SQLiteCGOStore{
		db:                      db,
		path:                    dbPath,
		prefix:                  cfg.prefix,
		retentionPeriod:         cfg.retentionPeriod,
		retentionSweepInterval:  cfg.retentionSweepInterval,
		disableRetentionSweeper: cfg.disableRetentionSweeper,
		retentionStop:           make(chan struct{}),
	}
	if err := s.Ping(); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	if err := s.applySchema(context.Background()); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	if err := s.recoverSequence(context.Background()); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	s.startRetentionSweeper()
	return s, nil
}

func sqliteCGODSN(path string) string {
	return "file:" + path + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL&_foreign_keys=ON"
}

func (s *SQLiteCGOStore) applySchema(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA wal_autocheckpoint=1000`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE IF NOT EXISTS kv (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS beads (
			id TEXT PRIMARY KEY,
			tier TEXT NOT NULL CHECK (tier IN ('main','wisp')),
			title TEXT NOT NULL,
			status TEXT NOT NULL,
			issue_type TEXT NOT NULL,
			priority INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			assignee TEXT NOT NULL DEFAULT '',
			from_agent TEXT NOT NULL DEFAULT '',
			parent_id TEXT NOT NULL DEFAULT '',
			ref TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			bead_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS labels (
			bead_id TEXT NOT NULL,
			label TEXT NOT NULL,
			PRIMARY KEY(bead_id, label),
			FOREIGN KEY(bead_id) REFERENCES beads(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS metadata (
			bead_id TEXT NOT NULL,
			meta_key TEXT NOT NULL,
			meta_value TEXT NOT NULL,
			PRIMARY KEY(bead_id, meta_key),
			FOREIGN KEY(bead_id) REFERENCES beads(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS deps (
			issue_id TEXT NOT NULL,
			depends_on_id TEXT NOT NULL,
			dep_type TEXT NOT NULL,
			PRIMARY KEY(issue_id, depends_on_id, dep_type)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_tier_status ON beads(tier, status)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_type ON beads(issue_type)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_assignee ON beads(assignee)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_parent ON beads(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_created ON beads(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_updated ON beads(updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_labels_label ON labels(label)`,
		`CREATE INDEX IF NOT EXISTS idx_metadata_key_value ON metadata(meta_key, meta_value)`,
		`CREATE INDEX IF NOT EXISTS idx_deps_issue ON deps(issue_id)`,
		`CREATE INDEX IF NOT EXISTS idx_deps_depends ON deps(depends_on_id)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("applying sqlite-cgo schema: %w", err)
		}
	}
	return nil
}

func (s *SQLiteCGOStore) recoverSequence(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM beads WHERE id LIKE ?`, s.prefix+"-%")
	if err != nil {
		return fmt.Errorf("recovering sqlite-cgo sequence: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	maxSeq := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if n := numericIDSuffix(id); n > maxSeq {
			maxSeq = n
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO kv(key, value) VALUES('seq', ?)
		ON CONFLICT(key) DO UPDATE SET value =
			CASE WHEN CAST(value AS INTEGER) < CAST(excluded.value AS INTEGER)
			THEN excluded.value ELSE value END`,
		strconv.Itoa(maxSeq))
	if err != nil {
		return fmt.Errorf("saving sqlite-cgo sequence: %w", err)
	}
	return nil
}

// StoreHealthPath returns the SQLite database file path.
func (s *SQLiteCGOStore) StoreHealthPath() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Ping verifies that the SQLite store is reachable.
func (s *SQLiteCGOStore) Ping() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("sqlite-cgo store is closed")
	}
	if err := s.db.PingContext(context.Background()); err != nil {
		return fmt.Errorf("pinging sqlite-cgo store: %w", err)
	}
	return nil
}

// Create persists a new bead.
func (s *SQLiteCGOStore) Create(b Bead) (Bead, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Bead{}, fmt.Errorf("sqlite-cgo create: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	stored, err := s.normalizeCreate(ctx, tx, b)
	if err != nil {
		return Bead{}, err
	}
	if err := s.ensureCreateDoesNotExist(ctx, tx, stored.ID); err != nil {
		return Bead{}, err
	}
	if err := s.upsertBeadTx(ctx, tx, stored); err != nil {
		return Bead{}, err
	}
	for _, dep := range depsFromNeeds(stored) {
		if err := s.depAddTx(ctx, tx, dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
			return Bead{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Bead{}, fmt.Errorf("sqlite-cgo create: commit: %w", err)
	}
	return cloneBead(stored), nil
}

func (s *SQLiteCGOStore) ensureCreateDoesNotExist(ctx context.Context, tx *sql.Tx, id string) error {
	var found int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM beads WHERE id=?`, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking duplicate sqlite-cgo bead %q: %w", id, err)
	}
	return fmt.Errorf("creating bead %q: duplicate id", id)
}

func (s *SQLiteCGOStore) normalizeCreate(ctx context.Context, tx *sql.Tx, b Bead) (Bead, error) {
	b = cloneBead(b)
	if b.ID == "" {
		id, err := s.nextID(ctx, tx)
		if err != nil {
			return Bead{}, err
		}
		b.ID = id
	} else if n := numericIDSuffix(b.ID); n > 0 {
		if err := s.ensureSequenceAtLeast(ctx, tx, n); err != nil {
			return Bead{}, err
		}
	}
	if b.Status == "" {
		b.Status = "open"
	}
	if b.Type == "" {
		b.Type = "task"
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = b.CreatedAt
	}
	return b, nil
}

func (s *SQLiteCGOStore) nextID(ctx context.Context, tx *sql.Tx) (string, error) {
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM kv WHERE key='seq'`).Scan(&raw); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("sqlite-cgo allocate id: %w", err)
		}
		raw = "0"
	}
	seq, err := strconv.Atoi(raw)
	if err != nil {
		return "", fmt.Errorf("sqlite-cgo allocate id: parse seq %q: %w", raw, err)
	}
	seq++
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO kv(key, value) VALUES('seq', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.Itoa(seq)); err != nil {
		return "", fmt.Errorf("sqlite-cgo allocate id: save seq: %w", err)
	}
	return fmt.Sprintf("%s-%d", s.prefix, seq), nil
}

func (s *SQLiteCGOStore) ensureSequenceAtLeast(ctx context.Context, tx *sql.Tx, seq int) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO kv(key, value) VALUES('seq', ?)
		ON CONFLICT(key) DO UPDATE SET value =
			CASE WHEN CAST(value AS INTEGER) < CAST(excluded.value AS INTEGER)
			THEN excluded.value ELSE value END`,
		strconv.Itoa(seq))
	if err != nil {
		return fmt.Errorf("sqlite-cgo save imported id sequence: %w", err)
	}
	return nil
}

func (s *SQLiteCGOStore) upsertBeadTx(ctx context.Context, tx *sql.Tx, b Bead) error {
	payload, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("sqlite-cgo marshal bead %q: %w", b.ID, err)
	}
	tier := "main"
	if b.Ephemeral {
		tier = "wisp"
	}
	var priority any
	if b.Priority != nil {
		priority = *b.Priority
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO beads(id,tier,title,status,issue_type,priority,created_at,updated_at,assignee,from_agent,parent_id,ref,description,bead_json)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			tier=excluded.tier,
			title=excluded.title,
			status=excluded.status,
			issue_type=excluded.issue_type,
			priority=excluded.priority,
			created_at=excluded.created_at,
			updated_at=excluded.updated_at,
			assignee=excluded.assignee,
			from_agent=excluded.from_agent,
			parent_id=excluded.parent_id,
			ref=excluded.ref,
			description=excluded.description,
			bead_json=excluded.bead_json`,
		b.ID, tier, b.Title, b.Status, b.Type, priority, b.CreatedAt.UnixNano(), sqliteUnixNanoOrZero(b.UpdatedAt),
		b.Assignee, b.From, b.ParentID, b.Ref, b.Description, string(payload))
	if err != nil {
		return fmt.Errorf("sqlite-cgo upsert bead %q: %w", b.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE bead_id=?`, b.ID); err != nil {
		return fmt.Errorf("sqlite-cgo replace labels for %q: %w", b.ID, err)
	}
	for _, label := range b.Labels {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO labels(bead_id,label) VALUES(?,?)`, b.ID, label); err != nil {
			return fmt.Errorf("sqlite-cgo insert label for %q: %w", b.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metadata WHERE bead_id=?`, b.ID); err != nil {
		return fmt.Errorf("sqlite-cgo replace metadata for %q: %w", b.ID, err)
	}
	for k, v := range b.Metadata {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(bead_id,meta_key,meta_value) VALUES(?,?,?)`, b.ID, k, v); err != nil {
			return fmt.Errorf("sqlite-cgo insert metadata for %q: %w", b.ID, err)
		}
	}
	return nil
}

func sqliteUnixNanoOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// Get retrieves a bead by ID.
func (s *SQLiteCGOStore) Get(id string) (Bead, error) {
	row := s.db.QueryRowContext(context.Background(), `SELECT bead_json FROM beads WHERE id=?`, id)
	b, err := scanSQLiteCGOBead(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, err)
	}
	return b, nil
}

type sqliteCGOScanner interface {
	Scan(dest ...any) error
}

func scanSQLiteCGOBead(row sqliteCGOScanner) (Bead, error) {
	var raw string
	if err := row.Scan(&raw); err != nil {
		return Bead{}, err
	}
	var b Bead
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return Bead{}, err
	}
	return cloneBead(b), nil
}

// Update modifies fields of an existing bead.
func (s *SQLiteCGOStore) Update(id string, opts UpdateOpts) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite-cgo update: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	b, err := s.getTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if opts.Title != nil {
		b.Title = *opts.Title
	}
	if opts.Status != nil {
		b.Status = *opts.Status
	}
	if opts.Type != nil {
		b.Type = *opts.Type
	}
	if opts.Priority != nil {
		b.Priority = cloneIntPtr(opts.Priority)
	}
	if opts.Description != nil {
		b.Description = *opts.Description
	}
	if opts.ParentID != nil {
		b.ParentID = *opts.ParentID
	}
	if opts.Assignee != nil {
		b.Assignee = *opts.Assignee
	}
	if len(opts.Metadata) > 0 {
		if b.Metadata == nil {
			b.Metadata = make(map[string]string, len(opts.Metadata))
		}
		for k, v := range opts.Metadata {
			b.Metadata[k] = v
		}
	}
	if len(opts.Labels) > 0 {
		b.Labels = append(b.Labels, opts.Labels...)
	}
	if len(opts.RemoveLabels) > 0 {
		remove := make(map[string]bool, len(opts.RemoveLabels))
		for _, label := range opts.RemoveLabels {
			remove[label] = true
		}
		filtered := b.Labels[:0]
		for _, label := range b.Labels {
			if !remove[label] {
				filtered = append(filtered, label)
			}
		}
		b.Labels = filtered
	}
	b.UpdatedAt = time.Now()
	if err := s.upsertBeadTx(ctx, tx, b); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteCGOStore) getTx(ctx context.Context, tx *sql.Tx, id string) (Bead, error) {
	row := tx.QueryRowContext(ctx, `SELECT bead_json FROM beads WHERE id=?`, id)
	b, err := scanSQLiteCGOBead(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
	}
	return b, err
}

// Close sets a bead's status to closed.
func (s *SQLiteCGOStore) Close(id string) error {
	b, err := s.Get(id)
	if err != nil {
		return fmt.Errorf("closing bead %q: %w", id, err)
	}
	if b.Status == "closed" {
		return nil
	}
	status := "closed"
	return s.Update(id, UpdateOpts{Status: &status})
}

// Reopen sets a bead's status to open.
func (s *SQLiteCGOStore) Reopen(id string) error {
	b, err := s.Get(id)
	if err != nil {
		return fmt.Errorf("reopening bead %q: %w", id, err)
	}
	if b.Status == "open" {
		return nil
	}
	status := "open"
	return s.Update(id, UpdateOpts{Status: &status})
}

// CloseAll closes multiple beads and applies metadata to each closed bead.
func (s *SQLiteCGOStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		b, err := s.Get(id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return closed, err
		}
		if b.Status == "closed" {
			continue
		}
		opts := UpdateOpts{Status: ptrString("closed"), Metadata: maps.Clone(metadata)}
		if err := s.Update(id, opts); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// List returns beads matching the query.
func (s *SQLiteCGOStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	sqlText, args := sqliteCGOListSQL(query)
	rows, err := s.db.QueryContext(context.Background(), sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("listing sqlite-cgo beads: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var result []Bead
	for rows.Next() {
		b, err := scanSQLiteCGOBead(rows)
		if err != nil {
			return nil, fmt.Errorf("listing sqlite-cgo beads: %w", err)
		}
		result = append(result, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing sqlite-cgo beads: %w", err)
	}
	return result, nil
}

func sqliteCGOListSQL(q ListQuery) (string, []any) {
	where := []string{}
	args := []any{}
	switch q.TierMode {
	case TierWisps:
		where = append(where, "tier='wisp'")
	case TierBoth:
	default:
		where = append(where, "tier='main'")
	}
	if q.Status != "" {
		where = append(where, "status=?")
		args = append(args, q.Status)
	} else if !q.IncludeClosed {
		where = append(where, "status <> 'closed'")
	}
	if q.Type != "" {
		where = append(where, "issue_type=?")
		args = append(args, q.Type)
	}
	if q.Assignee != "" {
		where = append(where, "assignee=?")
		args = append(args, q.Assignee)
	}
	if q.ParentID != "" {
		where = append(where, "parent_id=?")
		args = append(args, q.ParentID)
	}
	if !q.CreatedBefore.IsZero() {
		where = append(where, "created_at < ?")
		args = append(args, q.CreatedBefore.UnixNano())
	}
	if !q.UpdatedBefore.IsZero() {
		where = append(where, "COALESCE(NULLIF(updated_at, 0), created_at) < ?")
		args = append(args, q.UpdatedBefore.UnixNano())
	}
	if q.Label != "" {
		where = append(where, "EXISTS (SELECT 1 FROM labels l WHERE l.bead_id=beads.id AND l.label=?)")
		args = append(args, q.Label)
	}
	for k, v := range q.Metadata {
		where = append(where, "EXISTS (SELECT 1 FROM metadata m WHERE m.bead_id=beads.id AND m.meta_key=? AND m.meta_value=?)")
		args = append(args, k, v)
	}
	sqlText := "SELECT bead_json FROM beads"
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	switch q.Sort {
	case SortCreatedAsc:
		sqlText += " ORDER BY created_at ASC, id ASC"
	case SortCreatedDesc:
		sqlText += " ORDER BY created_at DESC, id DESC"
	}
	if q.Limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	return sqlText, args
}

// ListOpen returns non-closed beads in creation order by default.
func (s *SQLiteCGOStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true, Sort: SortCreatedAsc}
	if len(status) > 0 {
		query.Status = status[0]
	}
	return s.List(query)
}

// Ready returns open, unblocked actionable main-tier beads.
func (s *SQLiteCGOStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(query)
	args := []any{}
	sqlText := `SELECT b.bead_json FROM beads b
		WHERE b.tier='main'
		  AND b.status='open'
		  AND b.issue_type NOT IN ('merge-request','gate','molecule','step','message','session','agent','role','rig')
		  AND NOT EXISTS (
			SELECT 1 FROM deps d
			LEFT JOIN beads blocker ON blocker.id=d.depends_on_id
			WHERE d.issue_id=b.id
			  AND d.dep_type IN ('blocks','waits-for','conditional-blocks')
			  AND COALESCE(blocker.status, '') <> 'closed'
		  )`
	if q.Assignee != "" {
		sqlText += " AND b.assignee=?"
		args = append(args, q.Assignee)
	}
	sqlText += " ORDER BY b.created_at ASC, b.id ASC"
	if q.Limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := s.db.QueryContext(context.Background(), sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("listing sqlite-cgo ready beads: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var result []Bead
	for rows.Next() {
		b, err := scanSQLiteCGOBead(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, rows.Err()
}

// Children returns all non-closed beads whose ParentID matches the given ID.
func (s *SQLiteCGOStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedAsc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByLabel returns non-closed beads matching an exact label string by
// default.
func (s *SQLiteCGOStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to the given agent with the specified
// status.
func (s *SQLiteCGOStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{
		Assignee: assignee,
		Status:   status,
		Limit:    limit,
		Sort:     SortCreatedDesc,
	})
}

// ListByMetadata returns beads whose metadata contains all key-value pairs.
func (s *SQLiteCGOStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// SetMetadata sets a key-value metadata pair on a bead.
func (s *SQLiteCGOStore) SetMetadata(id, key, value string) error {
	return s.SetMetadataBatch(id, map[string]string{key: value})
}

// SetMetadataBatch atomically sets multiple metadata keys on a bead.
func (s *SQLiteCGOStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	return s.Update(id, UpdateOpts{Metadata: maps.Clone(kvs)})
}

// Tx executes fn sequentially against the store.
func (s *SQLiteCGOStore) Tx(_ string, fn func(tx Tx) error) error {
	return runSequentialTx(s, fn)
}

// Delete permanently removes a bead and its indexed rows.
func (s *SQLiteCGOStore) Delete(id string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("sqlite-cgo delete: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.Exec(`DELETE FROM beads WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("deleting bead %q: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("deleting bead %q: %w", id, ErrNotFound)
	}
	if _, err := tx.Exec(`DELETE FROM deps WHERE issue_id=? OR depends_on_id=?`, id, id); err != nil {
		return fmt.Errorf("deleting bead %q deps: %w", id, err)
	}
	return tx.Commit()
}

// DepAdd records a dependency edge.
func (s *SQLiteCGOStore) DepAdd(issueID, dependsOnID, depType string) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("sqlite-cgo dep add: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := s.depAddTx(context.Background(), tx, issueID, dependsOnID, depType); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteCGOStore) depAddTx(ctx context.Context, tx *sql.Tx, issueID, dependsOnID, depType string) error {
	if depType == "" {
		depType = "blocks"
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO deps(issue_id, depends_on_id, dep_type) VALUES(?,?,?)
		ON CONFLICT(issue_id, depends_on_id, dep_type) DO NOTHING`,
		issueID, dependsOnID, depType)
	if err != nil {
		return fmt.Errorf("adding dependency %s -> %s: %w", issueID, dependsOnID, err)
	}
	return nil
}

// DepRemove removes a dependency edge.
func (s *SQLiteCGOStore) DepRemove(issueID, dependsOnID string) error {
	_, err := s.db.ExecContext(context.Background(), `DELETE FROM deps WHERE issue_id=? AND depends_on_id=?`, issueID, dependsOnID)
	return err
}

// DepList returns dependency edges for a bead.
func (s *SQLiteCGOStore) DepList(id, direction string) ([]Dep, error) {
	col := "issue_id"
	if direction == "up" {
		col = "depends_on_id"
	}
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT issue_id, depends_on_id, dep_type FROM deps WHERE `+col+`=?`,
		id)
	if err != nil {
		return nil, fmt.Errorf("listing dependencies for %q: %w", id, err)
	}
	defer rows.Close() //nolint:errcheck
	var out []Dep
	for rows.Next() {
		var d Dep
		if err := rows.Scan(&d.IssueID, &d.DependsOnID, &d.Type); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *SQLiteCGOStore) startRetentionSweeper() {
	if s.disableRetentionSweeper || s.retentionPeriod <= 0 || s.retentionSweepInterval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(s.retentionSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_, _ = s.purgeTerminal(context.Background(), s.retentionPeriod)
			case <-s.retentionStop:
				return
			}
		}
	}()
}

func (s *SQLiteCGOStore) purgeTerminal(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-olderThan).UnixNano()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM beads
		WHERE tier='main'
		  AND status IN ('closed','cancelled','canceled','expired')
		  AND COALESCE(NULLIF(updated_at,0), created_at) < ?
		ORDER BY updated_at ASC
		LIMIT 1000`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("sqlite-cgo purge terminal query: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close() //nolint:errcheck
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.Delete(id); err != nil && !errors.Is(err, ErrNotFound) {
			return 0, err
		}
	}
	return len(ids), nil
}

func ptrString(v string) *string {
	return &v
}
