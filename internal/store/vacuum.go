package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// This file gives the SQLite file back the space the store no longer uses
// (Story 4.13).
//
// SQLite never returns freed pages to the filesystem on its own. A deleted
// row's pages go on the freelist, later writes reuse them, and the file stays
// at its high-water mark until something runs VACUUM — which rewrites the
// whole database into a fresh copy and replaces the original with it. The
// store opens SQLite with auto_vacuum off, so the migration that moved every
// document into the bucket (Story 8.4) and every prune since have left the
// file as large as it ever was.
//
// PostgreSQL and MySQL reclaim space themselves, and wsaw does not tune them:
// Vacuum refuses a server dialect with ErrVacuumUnsupported rather than
// reporting a vacuum of zero bytes that never happened (Tenet 5).

// MaintenanceKindVacuum is the receipt kind of a vacuum run.
const MaintenanceKindVacuum = "vacuum"

// Vacuum outcomes, as VacuumStats.Outcome reports them.
const (
	// VacuumVacuumed is a vacuum that rewrote the file.
	VacuumVacuumed = "vacuumed"
	// VacuumSkipped is a vacuum that found too little free space to be worth
	// rewriting the file for, and rewrote nothing.
	VacuumSkipped = "skipped"
	// VacuumRefused is a vacuum that did not start because the disk could not
	// hold the copy VACUUM builds.
	VacuumRefused = "refused"
	// VacuumError is a vacuum that failed or was interrupted. VACUUM is
	// transactional, so the original file is intact either way.
	VacuumError = "error"
)

// DefaultVacuumMinFreeRatio is the share of the file that has to be free
// pages before a vacuum rewrites it, unless it is forced.
const DefaultVacuumMinFreeRatio = 0.2

// ErrVacuumUnsupported refuses a vacuum of a store that is not SQLite.
var ErrVacuumUnsupported = errors.New(
	"only a SQLite store is vacuumed by wsaw; PostgreSQL and MySQL reclaim space on their own",
)

// ErrVacuumNoSpace refuses a vacuum that would run out of disk part way.
var ErrVacuumNoSpace = errors.New("not enough free disk space to vacuum")

// ErrDatabaseLocked reports a vacuum that could not take the database's write
// lock within the busy timeout, which from a shell almost always means a
// running daemon is writing to it.
var ErrDatabaseLocked = errors.New("the database is locked by another connection")

// VacuumOptions configures one vacuum.
type VacuumOptions struct {
	// MinFreeRatio is the share of pages that must be on the freelist before
	// the file is rewritten. Zero takes DefaultVacuumMinFreeRatio.
	MinFreeRatio float64
	// Force rewrites the file whatever the free ratio: an operator who types
	// the command has already decided.
	Force bool
}

func (o VacuumOptions) minFreeRatio() float64 {
	if o.MinFreeRatio > 0 {
		return o.MinFreeRatio
	}

	return DefaultVacuumMinFreeRatio
}

// VacuumStats reports what one vacuum did, or what a plan found.
type VacuumStats struct {
	Outcome string `json:"outcome"`

	// Path is the database file.
	Path string `json:"path"`

	PageSize        int64 `json:"pageSize"`
	PagesBefore     int64 `json:"pagesBefore"`
	FreePagesBefore int64 `json:"freePagesBefore"`
	PagesAfter      int64 `json:"pagesAfter,omitempty"`

	// FileBytesBefore and FileBytesAfter are the database file's size on
	// disk, WAL excluded; WALBytesBefore and WALBytesAfter are the WAL's.
	FileBytesBefore int64 `json:"fileBytesBefore"`
	FileBytesAfter  int64 `json:"fileBytesAfter,omitempty"`
	WALBytesBefore  int64 `json:"walBytesBefore"`
	WALBytesAfter   int64 `json:"walBytesAfter,omitempty"`

	// FreeRatio is FreePagesBefore / PagesBefore, and MinFreeRatio the
	// threshold it was held against: together they are why a vacuum was
	// skipped, or why it was not.
	FreeRatio    float64 `json:"freeRatio"`
	MinFreeRatio float64 `json:"minFreeRatio"`
	Forced       bool    `json:"forced,omitempty"`

	// BytesNeeded and BytesAvailable are the free-space check: the least
	// free space the rewrite needs, and the least any location it writes to
	// had.
	BytesNeeded    int64 `json:"bytesNeeded"`
	BytesAvailable int64 `json:"bytesAvailable"`

	// CheckpointIncomplete is set when the WAL could not be truncated after
	// the rewrite because another connection was still reading from it. The
	// space comes back at the next checkpoint, not now.
	CheckpointIncomplete bool `json:"checkpointIncomplete,omitempty"`

	Duration time.Duration `json:"durationNs"`
}

// ReclaimedBytes is how much smaller the database and its WAL are together
// than before the vacuum. Zero for anything but a vacuum that rewrote the file.
func (s VacuumStats) ReclaimedBytes() int64 {
	if s.Outcome != VacuumVacuumed {
		return 0
	}

	return max(0, s.FileBytesBefore+s.WALBytesBefore-s.FileBytesAfter-s.WALBytesAfter)
}

// ReclaimableBytes is what the freelist holds: roughly what a vacuum gives
// back, before the WAL is counted.
func (s VacuumStats) ReclaimableBytes() int64 { return s.FreePagesBefore * s.PageSize }

// VacuumStats decodes a vacuum run's own stats.
func (r MaintenanceRun) VacuumStats() (VacuumStats, error) {
	var s VacuumStats

	err := json.Unmarshal(r.Stats, &s)

	return s, err
}

// RecordVacuumRun records a completed Vacuum. A plan is never recorded.
func (s *SQL) RecordVacuumRun(
	ctx context.Context, trigger string, started time.Time, stats VacuumStats, runErr error,
) error {
	return s.recordMaintenanceRun(ctx, MaintenanceKindVacuum, trigger, started, stats, runErr)
}

// SupportsVacuum reports whether Vacuum can do anything for this store, which
// is whether it is SQLite. The daemon asks before scheduling one.
func (s *SQL) SupportsVacuum() bool { return s.d.name() == DriverSQLite }

// Vacuum rewrites the database file without its free pages and truncates the
// WAL, unless too little of the file is free to be worth it (opts), or the
// disk cannot hold the copy the rewrite builds. It records its own receipt as
// the last thing it does, whatever the outcome, the contract Prune and Sweep
// keep (Story 4.11).
//
// A store that is not SQLite is refused with ErrVacuumUnsupported and no
// receipt: nothing was attempted.
func (s *SQL) Vacuum(ctx context.Context, trigger string, opts VacuumOptions) (stats VacuumStats, err error) {
	if !s.SupportsVacuum() {
		return VacuumStats{}, fmt.Errorf("vacuum %s: %w", s.d.name(), ErrVacuumUnsupported)
	}

	started := time.Now()

	defer func() {
		stats.Duration = time.Since(started)

		// A refusal is its own outcome. Any other failure is an error, even
		// one that came after the plan had said "go ahead": the outcome
		// reports what happened, not what was intended.
		if err != nil && stats.Outcome != VacuumRefused {
			stats.Outcome = VacuumError
		}

		recCtx, cancel := receiptCtx(ctx)
		defer cancel()

		if recErr := s.RecordVacuumRun(recCtx, trigger, started, stats, err); recErr != nil {
			s.log.Warn("a vacuum run could not be recorded", "error", recErr)
		}
	}()

	stats, err = s.planVacuum(ctx, opts)
	if err != nil || stats.Outcome != VacuumVacuumed {
		return stats, err
	}

	if err := s.vacuum(ctx, &stats); err != nil {
		return stats, err
	}

	return stats, nil
}

// PlanVacuum reports what Vacuum would do and writes nothing: the page
// counts, what the freelist holds, whether the threshold would let the
// rewrite happen, and whether the disk could hold it. Its Outcome is the one
// Vacuum would reach before rewriting anything.
func (s *SQL) PlanVacuum(ctx context.Context, opts VacuumOptions) (VacuumStats, error) {
	if !s.SupportsVacuum() {
		return VacuumStats{}, fmt.Errorf("vacuum %s: %w", s.d.name(), ErrVacuumUnsupported)
	}

	return s.planVacuum(ctx, opts)
}

// planVacuum reads the file's shape and decides. Outcome is VacuumVacuumed
// when the rewrite should go ahead, VacuumSkipped or VacuumRefused when it
// should not; a refusal also returns an error wrapping ErrVacuumNoSpace.
func (s *SQL) planVacuum(ctx context.Context, opts VacuumOptions) (VacuumStats, error) {
	stats := VacuumStats{MinFreeRatio: opts.minFreeRatio(), Forced: opts.Force}

	if err := s.readVacuumShape(ctx, &stats); err != nil {
		return stats, err
	}

	if !opts.Force && stats.FreeRatio < stats.MinFreeRatio {
		stats.Outcome = VacuumSkipped

		return stats, nil
	}

	if err := s.checkVacuumSpace(&stats); err != nil {
		return stats, err
	}

	stats.Outcome = VacuumVacuumed

	return stats, nil
}

// readVacuumShape fills in the page counts and file sizes.
func (s *SQL) readVacuumShape(ctx context.Context, stats *VacuumStats) error {
	opCtx, cancel := opCtxFrom(ctx)
	defer cancel()

	path, err := s.databaseFile(opCtx)
	if err != nil {
		return err
	}

	stats.Path = path

	for _, p := range []struct {
		pragma string
		into   *int64
	}{
		{"page_size", &stats.PageSize},
		{"page_count", &stats.PagesBefore},
		{"freelist_count", &stats.FreePagesBefore},
	} {
		if err := s.db.QueryRowContext(opCtx, "pragma "+p.pragma).Scan(p.into); err != nil {
			return fmt.Errorf("reading pragma %s: %w", p.pragma, err)
		}
	}

	if stats.PagesBefore > 0 {
		stats.FreeRatio = float64(stats.FreePagesBefore) / float64(stats.PagesBefore)
	}

	stats.FileBytesBefore, stats.WALBytesBefore, err = databaseSizes(path)

	return err
}

// databaseFile is the path SQLite has open as the main database. It is read
// from SQLite rather than kept from the options, so the file measured is the
// file that will be rewritten.
func (s *SQL) databaseFile(ctx context.Context) (string, error) {
	rows, err := s.db.QueryContext(ctx, "pragma database_list")
	if err != nil {
		return "", fmt.Errorf("reading the database file: %w", err)
	}

	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			seq        int
			name, file string
		)

		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", fmt.Errorf("reading the database file: %w", err)
		}

		if name == "main" && file != "" {
			return file, nil
		}
	}

	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("reading the database file: %w", err)
	}

	return "", errors.New("reading the database file: SQLite reports no file for the main database")
}

// databaseSizes is the size of the database file and of its WAL. A missing
// WAL is an empty one: SQLite removes it when the last connection closes.
func databaseSizes(path string) (file, wal int64, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("measuring the database file: %w", err)
	}

	wi, err := os.Stat(path + "-wal")

	switch {
	case errors.Is(err, os.ErrNotExist):
		return fi.Size(), 0, nil
	case err != nil:
		return 0, 0, fmt.Errorf("measuring the WAL: %w", err)
	}

	return fi.Size(), wi.Size(), nil
}

// vacuumSpaceMargin is headroom on top of the live data, for the pages
// SQLite's own bookkeeping adds and for whatever else writes to the same disk
// while the rewrite runs.
const vacuumSpaceMargin = 1.1

// checkVacuumSpace refuses a rewrite the disk cannot hold.
//
// VACUUM writes the live data twice. It first builds a copy in a temporary
// file in SQLite's temp directory, and then — in WAL mode — writes every page
// of that copy into the WAL beside the database, before a checkpoint moves
// them into the file and truncates both. Each location needs about the live
// data free; where they are the same filesystem, it needs twice that.
func (s *SQL) checkVacuumSpace(stats *VacuumStats) error {
	live := (stats.PagesBefore - stats.FreePagesBefore) * stats.PageSize
	perCopy := int64(float64(live) * vacuumSpaceMargin)

	need, err := vacuumNeeds(filepath.Dir(stats.Path), sqliteTempDir(), perCopy)
	if err != nil {
		return err
	}

	stats.BytesAvailable = -1

	for dir, bytes := range need {
		free, err := s.freeSpace(dir)
		if err != nil {
			return fmt.Errorf("checking free space in %s: %w", dir, err)
		}

		stats.BytesNeeded = max(stats.BytesNeeded, bytes)

		if stats.BytesAvailable < 0 || free < stats.BytesAvailable {
			stats.BytesAvailable = free
		}

		if free < bytes {
			stats.Outcome = VacuumRefused
			stats.BytesNeeded = bytes
			stats.BytesAvailable = free

			return fmt.Errorf("%w: %s needs %d bytes free and has %d", ErrVacuumNoSpace, dir, bytes, free)
		}
	}

	return nil
}

// vacuumNeeds is how many bytes each directory VACUUM writes to must have
// free, with the two folded into one when they are on the same filesystem.
func vacuumNeeds(dbDir, tempDir string, perCopy int64) (map[string]int64, error) {
	same, err := sameFilesystem(dbDir, tempDir)
	if err != nil {
		return nil, err
	}

	if same {
		return map[string]int64{dbDir: 2 * perCopy}, nil
	}

	return map[string]int64{dbDir: perCopy, tempDir: perCopy}, nil
}

func sameFilesystem(a, b string) (bool, error) {
	ai, err := os.Stat(a)
	if err != nil {
		return false, fmt.Errorf("checking free space: %w", err)
	}

	bi, err := os.Stat(b)
	if err != nil {
		return false, fmt.Errorf("checking free space: %w", err)
	}

	as, aok := ai.Sys().(*syscall.Stat_t)
	bs, bok := bi.Sys().(*syscall.Stat_t)

	// Without a device number to compare, assume the worst case: one
	// filesystem holding both copies.
	if !aok || !bok {
		return true, nil
	}

	return as.Dev == bs.Dev, nil
}

// sqliteTempDir is the directory SQLite writes VACUUM's temporary copy to. It
// follows SQLite's own search on Unix: SQLITE_TMPDIR, then TMPDIR, then the
// first of /var/tmp, /usr/tmp and /tmp that is a directory, then the current
// directory.
func sqliteTempDir() string {
	for _, env := range []string{"SQLITE_TMPDIR", "TMPDIR"} {
		if dir := os.Getenv(env); isDir(dir) {
			return dir
		}
	}

	for _, dir := range []string{"/var/tmp", "/usr/tmp", "/tmp"} {
		if isDir(dir) {
			return dir
		}
	}

	return "."
}

func isDir(path string) bool {
	if path == "" {
		return false
	}

	// #nosec G703 -- path is one of SQLite's own candidate temp directories,
	// from the process's environment or a fixed list, and is only stat'ed.
	fi, err := os.Stat(path)

	return err == nil && fi.IsDir()
}

// statfsFreeSpace is the space an unprivileged process may still write in
// the filesystem holding dir.
func statfsFreeSpace(dir string) (int64, error) {
	var st syscall.Statfs_t

	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}

	// Bsize is a signed type on Linux and an unsigned one on macOS, so the
	// conversion is written once for both; a block size is never negative,
	// and no filesystem has 2^63 bytes free.
	free := st.Bavail * uint64(st.Bsize) // #nosec G115 -- a block size, from the kernel
	if free > uint64(1<<63-1) {
		return 1<<63 - 1, nil
	}

	return int64(free), nil
}

// freeSpace is statfsFreeSpace unless a test has put a disk of its own
// choosing in its place.
func (s *SQL) freeSpace(dir string) (int64, error) {
	if s.freeSpaceFn != nil {
		return s.freeSpaceFn(dir)
	}

	return statfsFreeSpace(dir)
}

// vacuum rewrites the file and truncates the WAL.
//
// The statement has no deadline of its own: how long a rewrite takes is a
// function of the database's size, not something one timeout fits. It is bound
// by the caller's context instead, and the driver interrupts the statement
// when that is cancelled; VACUUM is a transaction, so an interrupted one
// leaves the original file exactly as it was. It is not retried: a lock that
// the busy timeout could not wait out is somebody else's long write, most
// likely a daemon's, and trying again at once would meet it again.
func (s *SQL) vacuum(ctx context.Context, stats *VacuumStats) error {
	if _, err := s.db.ExecContext(ctx, "vacuum"); err != nil {
		if ctx.Err() == nil && s.d.isTransient(err) {
			return fmt.Errorf("vacuum %s: %w: %w", stats.Path, ErrDatabaseLocked, err)
		}

		return fmt.Errorf("vacuum %s: %w", stats.Path, err)
	}

	opCtx, cancel := opCtxFrom(ctx)
	defer cancel()

	var busy, logPages, checkpointed int

	if err := s.db.QueryRowContext(opCtx, "pragma wal_checkpoint(TRUNCATE)").
		Scan(&busy, &logPages, &checkpointed); err != nil {
		return fmt.Errorf("truncating the WAL after vacuuming %s: %w", stats.Path, err)
	}

	stats.CheckpointIncomplete = busy != 0

	if err := s.db.QueryRowContext(opCtx, "pragma page_count").Scan(&stats.PagesAfter); err != nil {
		return fmt.Errorf("reading pragma page_count: %w", err)
	}

	var err error

	stats.FileBytesAfter, stats.WALBytesAfter, err = databaseSizes(stats.Path)

	return err
}
