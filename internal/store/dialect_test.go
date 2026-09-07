package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pflege-de-labs/wsaw/internal/secret"
)

// These are white-box tests of the seam itself (Story 4.7). They need no
// database, so they run in the fast suite whatever it is configured for.

// TestEveryDialectHasTheSameMigrations is AC4. The schema version is an index
// into this list, so a dialect with a different number of migrations would
// mean version 2 described two different schemas depending on the database —
// and a store would be migrated to the wrong one.
func TestEveryDialectHasTheSameMigrations(t *testing.T) {
	t.Parallel()

	want := len(sqliteDialect{}.migrations())

	if want == 0 {
		t.Fatal("the sqlite dialect has no migrations")
	}

	for _, name := range Drivers() {
		d, err := dialectFor(name)
		if err != nil {
			t.Fatal(err)
		}

		if got := len(d.migrations()); got != want {
			t.Errorf("%s has %d migrations, sqlite has %d: the schema version would mean different things per database",
				name, got, want)
		}
	}
}

// TestEveryDialectDeclaresTheSameTables catches a schema that drifts in
// substance rather than in count.
func TestEveryDialectDeclaresTheSameTables(t *testing.T) {
	t.Parallel()

	for _, name := range Drivers() {
		d, err := dialectFor(name)
		if err != nil {
			t.Fatal(err)
		}

		ddl := strings.ToLower(strings.Join(flatten(d.migrations()), "\n"))

		for _, table := range []string{"results", "baselines", "audit"} {
			if !strings.Contains(ddl, "table if not exists "+table) {
				t.Errorf("%s does not create the %s table", name, table)
			}
		}

		// The index every result query depends on. Without it, a listing
		// still works and quietly scans the table.
		if !strings.Contains(ddl, "results_series") {
			t.Errorf("%s does not create the results_series index", name)
		}
	}
}

func flatten(groups [][]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}

	return out
}

// TestUnknownDriverIsRefusedWithItsAlternatives is the difference between an
// operator fixing a typo in a minute and reading source code.
func TestUnknownDriverIsRefusedWithItsAlternatives(t *testing.T) {
	t.Parallel()

	_, err := dialectFor("cockroach")
	if err == nil {
		t.Fatal("an unknown driver was accepted")
	}

	for _, want := range Drivers() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q as an option: %v", want, err)
		}
	}
}

// TestServerDriversRequireADSN: a driver with nowhere to connect must fail at
// startup, not on the first scan.
func TestServerDriversRequireADSN(t *testing.T) {
	t.Parallel()

	for _, name := range []string{DriverPostgres, DriverMySQL} {
		d, err := dialectFor(name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := d.dsn(Options{}); err == nil {
			t.Errorf("%s accepted an empty DSN", name)
		} else if !strings.Contains(err.Error(), "store.dsn") {
			t.Errorf("%s does not say what is missing: %v", name, err)
		}
	}
}

func TestSQLiteRequiresAPath(t *testing.T) {
	t.Parallel()

	if _, err := (sqliteDialect{}).dsn(Options{}); err == nil {
		t.Error("the sqlite dialect accepted an empty path")
	}
}

// TestRebindDollar covers the PostgreSQL placeholder rewrite, including the
// case no current query has: a ? inside a string literal, which must not be
// renumbered into a parameter.
func TestRebindDollar(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"select 1":              "select 1",
		"where a = ? and b = ?": "where a = $1 and b = $2",
		"values (?, ?, ?)":      "values ($1, $2, $3)",
		"where a = ? and note = 'why?' and b = ?": "where a = $1 and note = 'why?' and b = $2",
	}

	for in, want := range cases {
		if got := rebindDollar(in); got != want {
			t.Errorf("rebindDollar(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUpsertFormsMatchTheirDatabase pins the two shapes. They are easy to get
// subtly wrong — an upsert that silently inserts a duplicate instead of
// overwriting would give one scan two rows.
func TestUpsertFormsMatchTheirDatabase(t *testing.T) {
	t.Parallel()

	shared := upsertExcluded(resultKey, resultUpdate)

	for _, name := range []string{DriverSQLite, DriverPostgres} {
		d, _ := dialectFor(name)

		if got := d.upsert(resultKey, resultUpdate); got != shared {
			t.Errorf("%s upsert = %q, want the shared on-conflict form", name, got)
		}
	}

	if !strings.Contains(shared, "on conflict (target, consent_mode, scan_id) do update set") {
		t.Errorf("the on-conflict form does not name the key: %q", shared)
	}

	if !strings.Contains(shared, "started_at = excluded.started_at") {
		t.Errorf("the on-conflict form does not overwrite started_at: %q", shared)
	}

	my, _ := dialectFor(DriverMySQL)

	mysqlForm := my.upsert(resultKey, resultUpdate)

	if !strings.Contains(mysqlForm, "on duplicate key update") {
		t.Errorf("the mysql upsert is not its own form: %q", mysqlForm)
	}

	if strings.Contains(mysqlForm, "values(") {
		t.Errorf("the mysql upsert uses the deprecated VALUES() function: %q", mysqlForm)
	}

	for _, col := range resultUpdate {
		if !strings.Contains(mysqlForm, col+" = new."+col) {
			t.Errorf("the mysql upsert does not overwrite %s: %q", col, mysqlForm)
		}
	}
}

// TestPruneByCountIsDialectSpecificOnlyWhereItMustBe records why MySQL has
// its own form, so that a later simplification has to confront the reason.
func TestPruneByCountIsDialectSpecificOnlyWhereItMustBe(t *testing.T) {
	t.Parallel()

	for _, name := range []string{DriverSQLite, DriverPostgres} {
		d, _ := dialectFor(name)

		if d.pruneByCount() != pruneByCountPortable {
			t.Errorf("%s does not use the portable prune; if it needs its own, say why in the dialect", name)
		}
	}

	my, _ := dialectFor(DriverMySQL)

	if my.pruneByCount() == pruneByCountPortable {
		t.Error("mysql uses the portable prune, which selects from the table it deletes from (error 1093)")
	}

	// Neither form may fall back to a physical row identifier: rowid and ctid
	// are not the same concept and MySQL has neither.
	for _, name := range Drivers() {
		d, _ := dialectFor(name)

		q := strings.ToLower(d.pruneByCount())

		for _, forbidden := range []string{"rowid", "ctid"} {
			if strings.Contains(q, forbidden) {
				t.Errorf("%s prunes by %s rather than by primary key", name, forbidden)
			}
		}
	}
}

// TestMySQLDSNForcesStrictMode: without it MySQL truncates an over-long value
// and reports success, which would shorten a captured URL and change the
// finding.
func TestMySQLDSNForcesStrictMode(t *testing.T) {
	t.Parallel()

	got, err := (mysqlDialect{}).dsn(Options{DSN: secret.Literal("user:pw@tcp(host:3306)/wsaw")})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got, "sql_mode") {
		t.Errorf("the mysql DSN does not set sql_mode: %q", got)
	}

	// An operator who has chosen their own sql_mode keeps it.
	explicit := "user:pw@tcp(host:3306)/wsaw?sql_mode=%27TRADITIONAL%27"

	got, err = (mysqlDialect{}).dsn(Options{DSN: secret.Literal(explicit)})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Count(got, "sql_mode") != 1 {
		t.Errorf("the mysql DSN overrode an explicit sql_mode: %q", got)
	}
}

// --- AC7: a lost connection is survivable ---------------------------------

// TestRetryClassification is what keeps the retry honest. Retrying a
// permanent failure makes it slower and hides its cause; not retrying a
// dropped connection loses an observation for no reason.
func TestRetryClassification(t *testing.T) {
	t.Parallel()

	transient := []error{
		errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
		errors.New("write tcp 127.0.0.1:5432: write: broken pipe"),
		errors.New("unexpected EOF"),
		driver.ErrBadConn,
	}

	permanent := []error{
		errors.New(`duplicate key value violates unique constraint "results_pkey"`),
		errors.New(`relation "results" does not exist`),
		errors.New("syntax error at or near \"slect\""),
	}

	for _, name := range Drivers() {
		d, err := dialectFor(name)
		if err != nil {
			t.Fatal(err)
		}

		if d.isTransient(nil) {
			t.Errorf("%s treats a nil error as transient", name)
		}

		for _, e := range transient {
			if !d.isTransient(e) {
				t.Errorf("%s does not retry %v", name, e)
			}
		}

		for _, e := range permanent {
			if d.isTransient(e) {
				t.Errorf("%s retries a permanent failure: %v", name, e)
			}
		}
	}
}

// TestPostgresReadsSQLSTATE: the message text is localised, the codes are not.
func TestPostgresReadsSQLSTATE(t *testing.T) {
	t.Parallel()

	d := postgresDialect{}

	for _, code := range []string{"08006", "08003", "40001", "40P01", "57P01", "53300"} {
		if !d.isTransient(&pgconn.PgError{Code: code}) {
			t.Errorf("SQLSTATE %s is not retried", code)
		}
	}

	// A unique violation is the statement's own fault.
	if d.isTransient(&pgconn.PgError{Code: "23505"}) {
		t.Error("a unique violation is retried")
	}
}

// TestMySQLReadsItsErrorNumbers is the same idea for MySQL.
func TestMySQLReadsItsErrorNumbers(t *testing.T) {
	t.Parallel()

	d := mysqlDialect{}

	for _, number := range []uint16{2006, 2013, 1213, 1205, 1053, 1040} {
		if !d.isTransient(&gomysql.MySQLError{Number: number}) {
			t.Errorf("MySQL error %d is not retried", number)
		}
	}

	if d.isTransient(&gomysql.MySQLError{Number: 1062}) {
		t.Error("a duplicate-entry error is retried")
	}
}

// TestSQLiteRetriesLockContentionOnly: there is no server to lose.
func TestSQLiteRetriesLockContentionOnly(t *testing.T) {
	t.Parallel()

	d := sqliteDialect{}

	if !d.isTransient(errors.New("database is locked (SQLITE_BUSY)")) {
		t.Error("SQLite does not retry lock contention")
	}

	if d.isTransient(errors.New("UNIQUE constraint failed: results.scan_id")) {
		t.Error("SQLite retries a constraint violation")
	}
}

// TestRetryStopsAtTheConfiguredLimit exercises the policy itself: how many
// attempts, whether it stops on a permanent error, and whether a cancelled
// caller is retried (it must not be).
func TestRetryStopsAtTheConfiguredLimit(t *testing.T) {
	t.Parallel()

	s := &Store{
		d:        sqliteDialect{},
		attempts: 4,
		backoff:  time.Millisecond,
	}

	var (
		attempts int
		seen     []int
	)

	s.onRetry = func(_ string, attempt int, _ error) { seen = append(seen, attempt) }

	locked := errors.New("database is locked")

	err := s.retry(context.Background(), "test", func(context.Context) error {
		attempts++

		return locked
	})

	if err == nil {
		t.Fatal("a permanently locked database eventually has to fail")
	}

	if attempts != 4 {
		t.Errorf("made %d attempts, want the configured 4", attempts)
	}

	if len(seen) != 3 {
		t.Errorf("reported %d retries, want 3 for 4 attempts", len(seen))
	}

	if !errors.Is(err, locked) {
		t.Errorf("the last error is not wrapped: %v", err)
	}

	// A permanent failure is returned at once.
	attempts = 0
	permanent := errors.New("UNIQUE constraint failed")

	if err := s.retry(context.Background(), "test", func(context.Context) error {
		attempts++

		return permanent
	}); !errors.Is(err, permanent) {
		t.Errorf("a permanent error was transformed: %v", err)
	}

	if attempts != 1 {
		t.Errorf("a permanent failure was attempted %d times, want 1", attempts)
	}

	// Success on a later attempt is a success, not a failure.
	attempts = 0

	if err := s.retry(context.Background(), "test", func(context.Context) error {
		attempts++

		if attempts < 3 {
			return locked
		}

		return nil
	}); err != nil {
		t.Errorf("an operation that recovered on the third attempt failed: %v", err)
	}
}

// TestRetryDoesNotOutliveItsContext: a cancelled caller is not a broken
// database, and retrying its work would only delay the shutdown it asked for.
func TestRetryDoesNotOutliveItsContext(t *testing.T) {
	t.Parallel()

	s := &Store{d: sqliteDialect{}, attempts: 100, backoff: time.Second}

	ctx, cancel := context.WithCancel(context.Background())

	attempts := 0

	err := s.retry(ctx, "test", func(context.Context) error {
		attempts++
		cancel()

		return errors.New("database is locked")
	})

	if err == nil {
		t.Fatal("a cancelled retry reported success")
	}

	if attempts != 1 {
		t.Errorf("kept retrying after cancellation: %d attempts", attempts)
	}
}

// TestOneAttemptMeansNoRetry: the policy has to be switchable off, for a
// deployment that would rather see the failure immediately.
func TestOneAttemptMeansNoRetry(t *testing.T) {
	t.Parallel()

	s := &Store{d: sqliteDialect{}, attempts: 1, backoff: time.Millisecond}

	attempts := 0

	if err := s.retry(context.Background(), "test", func(context.Context) error {
		attempts++

		return errors.New("database is locked")
	}); err == nil {
		t.Fatal("expected the failure to be returned")
	}

	if attempts != 1 {
		t.Errorf("made %d attempts with MaxAttempts=1", attempts)
	}
}

func TestRetryDefaults(t *testing.T) {
	t.Parallel()

	var opts Options

	if opts.maxAttempts() < 2 {
		t.Errorf("default attempts = %d; a server database needs at least one retry", opts.maxAttempts())
	}

	if opts.retryBackoff() <= 0 {
		t.Error("default backoff is not positive")
	}

	explicit := Options{MaxAttempts: 1, RetryBackoff: time.Minute}

	if explicit.maxAttempts() != 1 || explicit.retryBackoff() != time.Minute {
		t.Error("an explicit retry policy was overridden by the default")
	}
}
