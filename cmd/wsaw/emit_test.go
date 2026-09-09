package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/config"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/scanner"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// outcome builds a result that is complete enough for every exporter: a
// document request and a third-party request, in one consent mode.
func outcome(target string, mode model.ConsentMode) scanner.Outcome {
	at := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)

	return scanner.Outcome{Result: &model.Result{
		SchemaVersion: model.SchemaVersion,
		ScanID:        target + "-" + string(mode),
		Target:        target,
		URL:           "https://example.com/",
		ConsentMode:   mode,
		StartedAt:     at,
		FinishedAt:    at.Add(time.Second),
		Duration:      time.Second,
		Termination:   model.TermIdle,
		Consent:       model.Consent{Outcome: model.OutcomeApplied},
		Requests: []model.Request{
			{URL: "https://example.com/", NormalizedURL: "https://example.com/", Method: "GET",
				ResourceType: "document", Host: "example.com", Domain: "example.com",
				Party: model.FirstParty, Phase: model.PhasePre, Status: 200},
			{URL: "https://tracker.test/px", NormalizedURL: "https://tracker.test/px", Method: "GET",
				ResourceType: "image", Host: "tracker.test", Domain: "tracker.test",
				Party: model.ThirdParty, Phase: model.PhasePre, Status: 200},
		},
	}}
}

func TestSanitize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "an ordinary name is untouched", in: "example.com", want: "example.com"},
		{name: "separators become dashes", in: "a b/c", want: "a-b-c"},
		{name: "empty falls back", in: "", want: "target"},
		// The name reaches a filesystem path, and a target name can come from
		// a URL. Traversal must not survive it (Rule 2).
		{name: "traversal is neutralised", in: "../../etc/passwd", want: "..-..-etc-passwd"},
		{name: "a separator-only name yields no path", in: "///", want: "---"},
		{name: "a null byte becomes a dash", in: "a\x00b", want: "a-b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := sanitize(tc.in)
			if got != tc.want {
				t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}

			if strings.ContainsAny(got, `/\`) {
				t.Errorf("sanitize(%q) = %q, which still contains a path separator", tc.in, got)
			}
		})
	}
}

func TestSortOutcomesIsDeterministic(t *testing.T) {
	t.Parallel()

	outcomes := []scanner.Outcome{
		outcome("b", model.ConsentReject),
		outcome("a", model.ConsentReject),
		outcome("b", model.ConsentAccept),
		outcome("a", model.ConsentNone),
	}

	sortOutcomes(outcomes)

	var got []string
	for _, out := range outcomes {
		got = append(got, out.Result.Target+"/"+string(out.Result.ConsentMode))
	}

	want := []string{"a/none", "a/reject", "b/accept", "b/reject"}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestLessOutcomeOrdersByTargetThenMode(t *testing.T) {
	t.Parallel()

	a := outcome("a", model.ConsentReject)
	b := outcome("b", model.ConsentAccept)

	if !lessOutcome(a, b) {
		t.Error("target should decide before consent mode")
	}

	if lessOutcome(b, a) {
		t.Error("ordering is not antisymmetric")
	}

	accept, reject := outcome("a", model.ConsentAccept), outcome("a", model.ConsentReject)
	if !lessOutcome(accept, reject) {
		t.Error("within a target, the consent mode should decide")
	}
}

// TestRetryReasonExplainsWhyAnAttemptFailed matters because the reason is
// logged and handed to the next attempt: "the scan produced no result" and a
// termination reason are different diagnoses.
func TestRetryReasonExplainsWhyAnAttemptFailed(t *testing.T) {
	t.Parallel()

	failed := outcome("a", model.ConsentReject)
	failed.Result.Termination = model.TermError
	failed.Result.Error = "navigation failed"

	terminated := outcome("a", model.ConsentReject)
	terminated.Result.Termination = model.TermTimeout

	cases := []struct {
		name string
		out  scanner.Outcome
		err  error
		want string
	}{
		{name: "no result and an error", out: scanner.Outcome{}, err: errors.New("boom"), want: "boom"},
		{name: "no result at all", out: scanner.Outcome{}, want: "the scan produced no result"},
		{name: "a recorded error", out: failed, want: string(model.TermError) + ": navigation failed"},
		{name: "a termination reason", out: terminated, want: string(model.TermTimeout)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := retryReason(tc.out, tc.err); got != tc.want {
				t.Errorf("retryReason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEmitStdoutWritesEveryFormat(t *testing.T) {
	outcomes := []scanner.Outcome{outcome("a", model.ConsentReject), outcome("b", model.ConsentAccept)}

	cases := []struct {
		name   string
		format string
		want   string
	}{
		{name: "json", format: "json", want: `"scanId"`},
		{name: "jsonl", format: "jsonl", want: `"scanId"`},
		{name: "csv", format: "csv", want: "tracker.test"},
		{name: "markdown", format: "markdown", want: "example.com"},
		{name: "md is an alias", format: "md", want: "example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error

			stdout, _ := capture(t, func() {
				err = emitStdout(outcomes, tc.format, nil)
			})

			if err != nil {
				t.Fatalf("emitStdout(%q): %v", tc.format, err)
			}

			if !strings.Contains(stdout, tc.want) {
				t.Errorf("%s output does not contain %q; got:\n%s", tc.format, tc.want, stdout)
			}

			// Both outcomes have to appear, or an export silently drops half
			// of what was observed.
			for _, target := range []string{"a", "b"} {
				if !strings.Contains(stdout, target) {
					t.Errorf("%s output does not mention target %q", tc.format, target)
				}
			}
		})
	}
}

func TestEmitStdoutRejectsAnUnknownFormat(t *testing.T) {
	err := emitStdout(nil, "yaml", nil)
	if err == nil {
		t.Fatal("emitStdout accepted an unknown format")
	}

	if !strings.Contains(err.Error(), "yaml") {
		t.Errorf("error = %v, want it to name the format", err)
	}
}

// TestEmitWritesTheOutputDirectory covers the other half of emit: with an
// output directory configured, every enabled artefact is written per outcome.
func TestEmitWritesTheOutputDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")

	cfg := config.New()
	cfg.Store.OutputDir = dir
	cfg.Store.WriteReport = true
	cfg.Store.WriteHAR = true
	cfg.Store.WriteJSONL = true

	a := &app.App{Config: cfg}

	if err := emit(a, []scanner.Outcome{outcome("a b", model.ConsentReject)}, "json"); err != nil {
		t.Fatalf("emit: %v", err)
	}

	// The target name is sanitised on the way to a filename.
	base := filepath.Join(dir, "a-b-reject")

	for _, ext := range []string{".json", ".md", ".har", ".jsonl"} {
		info, err := os.Stat(base + ext)
		if err != nil {
			t.Errorf("%s was not written: %v", ext, err)

			continue
		}

		if info.Size() == 0 {
			t.Errorf("%s is empty", ext)
		}

		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has mode %o, want 600", ext, perm)
		}
	}
}

func TestEmitWritesOnlyWhatIsEnabled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")

	cfg := config.New()
	cfg.Store.OutputDir = dir

	a := &app.App{Config: cfg}

	if err := emit(a, []scanner.Outcome{outcome("a", model.ConsentReject)}, "json"); err != nil {
		t.Fatalf("emit: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "a-reject.json")); err != nil {
		t.Errorf("the result document is not optional: %v", err)
	}

	for _, ext := range []string{".md", ".har", ".jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, "a-reject"+ext)); err == nil {
			t.Errorf("%s was written without being configured", ext)
		}
	}
}

func TestEmitFallsBackToStdout(t *testing.T) {
	a := &app.App{Config: config.New()}

	var err error

	stdout, _ := capture(t, func() {
		err = emit(a, []scanner.Outcome{outcome("a", model.ConsentReject)}, "csv")
	})

	if err != nil {
		t.Fatalf("emit: %v", err)
	}

	if !strings.Contains(stdout, "tracker.test") {
		t.Errorf("stdout = %q, want the CSV export", stdout)
	}
}

func TestEmitReportsAnUnwritableOutputDirectory(t *testing.T) {
	// A file where the directory should go: MkdirAll cannot succeed.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.New()
	cfg.Store.OutputDir = blocked

	a := &app.App{Config: cfg}

	err := emit(a, []scanner.Outcome{outcome("a", model.ConsentReject)}, "json")
	if err == nil {
		t.Fatal("emit accepted an output directory it could not create")
	}

	if !strings.Contains(err.Error(), blocked) {
		t.Errorf("error = %v, want it to name the directory", err)
	}
}

// TestWriteAtomicLeavesNothingBehindOnFailure is the property the indirection
// exists for: a reader must never find a partial result and trust it.
func TestWriteAtomicLeavesNothingBehindOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.json")

	wantErr := errors.New("the writer gave up")

	err := writeAtomic(path, func(f *os.File) error {
		if _, writeErr := f.WriteString("half a document"); writeErr != nil {
			return writeErr
		}

		return wantErr
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}

	if _, err := os.Stat(path); err == nil {
		t.Error("a partial result was left at the destination")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".wsaw-") {
			t.Errorf("temporary file %s was left behind", e.Name())
		}
	}
}

func TestWriteAtomicPublishesTheWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")

	err := writeAtomic(path, func(f *os.File) error {
		_, err := f.WriteString("{}\n")

		return err
	})
	if err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(body) != "{}\n" {
		t.Errorf("contents = %q", body)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// A result can name third parties and carry response bodies, so it is not
	// world-readable.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestWriteAtomicReportsAnUnwritableDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "result.json")

	if err := writeAtomic(path, func(*os.File) error { return nil }); err == nil {
		t.Fatal("writeAtomic accepted a directory that does not exist")
	}
}

func TestBodyLoaderIsAbsentWithoutAStore(t *testing.T) {
	t.Parallel()

	if bodyLoader(&app.App{Config: config.New()}) != nil {
		t.Error("bodyLoader returned a loader with no store to load from")
	}
}

// TestBodyLoaderResolvesStoredBodies is what keeps an export from naming
// bodies that nothing in it can reach (Story 1.6, AC2).
func TestBodyLoaderResolvesStoredBodies(t *testing.T) {
	dir := t.TempDir()

	st, err := store.Open(store.Options{
		Path:        filepath.Join(dir, "wsaw.db"),
		ArtifactDir: filepath.Join(dir, "artifacts"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	ref, err := st.PutArtifact("body", []byte("console.log(1)"))
	if err != nil {
		t.Fatalf("storing the body: %v", err)
	}

	load := bodyLoader(&app.App{Config: config.New(), Store: st})
	if load == nil {
		t.Fatal("bodyLoader returned nothing with a store present")
	}

	body, err := load(ref)
	if err != nil {
		t.Fatalf("loading %s: %v", ref, err)
	}

	if string(body) != "console.log(1)" {
		t.Errorf("body = %q", body)
	}
}
