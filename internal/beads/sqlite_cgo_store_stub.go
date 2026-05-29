//go:build !cgo || !sqlite_cgo

package beads

import (
	"fmt"
	"time"
)

// SQLiteCGOStoreOptions configures the SQLite-CGo bead store.
type SQLiteCGOStoreOptions struct{}

// SQLiteCGOStoreOption customizes OpenSQLiteCGOStore.
type SQLiteCGOStoreOption func(*SQLiteCGOStoreOptions)

// SQLiteCGOStore is unavailable unless Gas City is built with
// -tags sqlite_cgo and CGo enabled.
type SQLiteCGOStore struct{}

// WithSQLiteCGOStoreIDPrefix sets the generated bead ID prefix.
func WithSQLiteCGOStoreIDPrefix(string) SQLiteCGOStoreOption {
	return func(*SQLiteCGOStoreOptions) {}
}

// WithSQLiteCGOStoreRetention configures terminal-record retention.
func WithSQLiteCGOStoreRetention(time.Duration, time.Duration) SQLiteCGOStoreOption {
	return func(*SQLiteCGOStoreOptions) {}
}

// OpenSQLiteCGOStore returns an error when the sqlite_cgo build tag is absent.
func OpenSQLiteCGOStore(string, ...SQLiteCGOStoreOption) (Store, error) {
	return nil, fmt.Errorf("coordstore provider requires a CGo build with -tags sqlite_cgo")
}
