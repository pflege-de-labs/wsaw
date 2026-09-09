package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pflege-de-labs/wsaw/internal/app"
	"github.com/pflege-de-labs/wsaw/internal/model"
	"github.com/pflege-de-labs/wsaw/internal/store"
)

// shareKey is 32 characters, which is the minimum the configuration accepts:
// a signing key shorter than that is refused rather than quietly weakened.
const shareKey = "0123456789abcdef0123456789abcdef"

// seedStore opens the store a command will later open by itself, writes one
// result into it, and closes it again. Sharing reads the store, so a scan has
// to exist before there is anything to share.
func seedStore(t *testing.T, path string, scanIDs ...string) {
	t.Helper()

	st, err := store.Open(store.Options{
		Path:        path,
		ArtifactDir: filepath.Join(filepath.Dir(path), "artifacts"),
	})
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}

	defer func() {
		if err := st.Close(); err != nil {
			t.Errorf("closing the store: %v", err)
		}
	}()

	for i, id := range scanIDs {
		out := outcome("site", model.ConsentReject)
		out.Result.ScanID = id
		out.Result.StartedAt = out.Result.StartedAt.Add(time.Duration(i) * time.Hour)
		out.Result.FinishedAt = out.Result.StartedAt.Add(time.Second)

		if err := st.PutResult(out.Result); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}
}

// storePath is where writeConfig puts the database.
func storePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "wsaw.db")
}

func TestSharePathFor(t *testing.T) {
	t.Parallel()

	got := sharePathFor("site", "reject", "scan-1")

	if want := "/shared/site/reject/scan-1"; got != want {
		t.Errorf("sharePathFor = %q, want %q", got, want)
	}
}

func TestTrimTrailingSlash(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{in: "https://a.test/", want: "https://a.test"},
		{in: "https://a.test///", want: "https://a.test"},
		{in: "https://a.test", want: "https://a.test"},
		{in: "", want: ""},
		{in: "///", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()

			if got := trimTrailingSlash(tc.in); got != tc.want {
				t.Errorf("trimTrailingSlash(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestLoadShareTargetResolvesLatest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.db")

	seedStore(t, path, "scan-1", "scan-2")

	st, err := store.Open(store.Options{Path: path, ArtifactDir: filepath.Join(dir, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	a := &app.App{Store: st}

	for _, scan := range []string{"", "latest"} {
		res, err := loadShareTarget(a, "site", model.ConsentReject, scan)
		if err != nil {
			t.Fatalf("loadShareTarget(%q): %v", scan, err)
		}

		if res.ScanID != "scan-2" {
			t.Errorf("scan ID = %q, want the newest scan", res.ScanID)
		}
	}

	res, err := loadShareTarget(a, "site", model.ConsentReject, "scan-1")
	if err != nil {
		t.Fatalf("loadShareTarget by ID: %v", err)
	}

	if res.ScanID != "scan-1" {
		t.Errorf("scan ID = %q, want the one that was asked for", res.ScanID)
	}
}

func TestLoadShareTargetReportsWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wsaw.db")

	seedStore(t, path, "scan-1")

	st, err := store.Open(store.Options{Path: path, ArtifactDir: filepath.Join(dir, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	a := &app.App{Store: st}

	cases := []struct {
		name string
		mode model.ConsentMode
		scan string
		want string
	}{
		{name: "unknown target", mode: model.ConsentReject, scan: "latest", want: "no scan to share"},
		{name: "unscanned mode", mode: model.ConsentAccept, scan: "latest", want: "no scan to share"},
		{name: "unknown scan ID", mode: model.ConsentReject, scan: "scan-9", want: "share:"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := "site"
			if tc.name == "unknown target" {
				target = "absent"
			}

			_, err := loadShareTarget(a, target, tc.mode, tc.scan)
			if err == nil {
				t.Fatal("loadShareTarget invented a scan")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestCmdShareMintsALink(t *testing.T) {
	path := writeConfig(t, oneTargetConfig+`
api:
  share:
    enabled: true
    key: "`+shareKey+`"
    baseUrl: https://wsaw.example.com/
`)

	seedStore(t, storePath(path), "scan-1")

	var err error

	stdout, stderr := capture(t, func() {
		err = cmdShare(t.Context(), []string{"--config", path, "--target", "site"})
	})

	if err != nil {
		t.Fatalf("cmdShare: %v", err)
	}

	link := strings.TrimSpace(stdout)

	// stdout carries the link and nothing else, so it can be piped.
	if strings.Count(link, "\n") != 0 {
		t.Errorf("stdout carries more than the link: %q", stdout)
	}

	if !strings.HasPrefix(link, "https://wsaw.example.com/shared/site/reject/scan-1?t=") {
		t.Errorf("link = %q", link)
	}

	// The warnings belong on stderr, and the one that cannot be left out is
	// that the link cannot be revoked (Story 5.19, AC12).
	if !strings.Contains(stderr, "cannot be revoked") {
		t.Errorf("stderr does not warn about revocation: %q", stderr)
	}
}

// TestCmdShareSaysWhenTheLinkIsOnlyAPath keeps an operator from sending
// something that is not sendable.
func TestCmdShareSaysWhenTheLinkIsOnlyAPath(t *testing.T) {
	path := writeConfig(t, oneTargetConfig+`
api:
  share:
    enabled: true
    key: "`+shareKey+`"
`)

	seedStore(t, storePath(path), "scan-1")

	var err error

	stdout, stderr := capture(t, func() {
		err = cmdShare(t.Context(), []string{"--config", path, "--target", "site", "--validity", "1h"})
	})

	if err != nil {
		t.Fatalf("cmdShare: %v", err)
	}

	if !strings.HasPrefix(strings.TrimSpace(stdout), "/shared/site/reject/scan-1?t=") {
		t.Errorf("link = %q, want a bare path", stdout)
	}

	if !strings.Contains(stderr, "this is a path, not a URL") {
		t.Errorf("stderr does not explain that the link is a path: %q", stderr)
	}
}

func TestCmdShareRefusesBadInvocations(t *testing.T) {
	enabled := writeConfig(t, oneTargetConfig+`
api:
  share:
    enabled: true
    key: "`+shareKey+`"
`)

	disabled := writeConfig(t, oneTargetConfig)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no target",
			args: []string{"--config", enabled},
			want: "--target is required",
		},
		{
			name: "invalid consent mode",
			args: []string{"--config", enabled, "--target", "site", "--mode", "maybe"},
			want: "--mode must be none, reject or accept",
		},
		{
			name: "sharing switched off",
			args: []string{"--config", disabled, "--target", "site"},
			want: "sharing is not enabled",
		},
		{
			name: "unknown flag",
			args: []string{"--nope"},
			want: "flag provided but not defined",
		},
		{
			name: "unreadable configuration",
			args: []string{"--config", filepath.Join(t.TempDir(), "absent.yaml"), "--target", "site"},
			want: "absent.yaml",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error

			capture(t, func() {
				err = cmdShare(t.Context(), tc.args)
			})

			if err == nil {
				t.Fatalf("cmdShare(%v) succeeded, want an error", tc.args)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestCmdShareRefusesAnUnresolvableKey covers the secret path: a key given as
// an environment reference that is not set must fail loudly, not sign with an
// empty string.
func TestCmdShareRefusesAnUnresolvableKey(t *testing.T) {
	path := writeConfig(t, oneTargetConfig+`
api:
  share:
    enabled: true
    key: "${env:WSAW_TEST_SHARE_KEY_ABSENT}"
`)

	seedStore(t, storePath(path), "scan-1")

	if _, ok := os.LookupEnv("WSAW_TEST_SHARE_KEY_ABSENT"); ok {
		t.Skip("WSAW_TEST_SHARE_KEY_ABSENT is set in this environment")
	}

	var err error

	capture(t, func() {
		err = cmdShare(t.Context(), []string{"--config", path, "--target", "site"})
	})

	if err == nil {
		t.Fatal("cmdShare signed a link with an unresolvable key")
	}
}
