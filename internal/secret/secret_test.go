package secret_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/martint17r/wsaw/internal/secret"
)

const plaintext = "hunter2-super-secret"

func TestResolveEnv(t *testing.T) {
	t.Setenv("WSAW_TEST_TOKEN", plaintext)

	v, err := secret.Resolve("${env:WSAW_TEST_TOKEN}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if !v.IsSet() {
		t.Fatal("IsSet() = false, want true")
	}

	if v.Reveal() != plaintext {
		t.Errorf("Reveal() = %q, want %q", v.Reveal(), plaintext)
	}
}

func TestResolveEnvMissing(t *testing.T) {
	t.Parallel()

	_, err := secret.Resolve("${env:WSAW_DEFINITELY_NOT_SET}")
	if !errors.Is(err, secret.ErrUnresolved) {
		t.Fatalf("err = %v, want ErrUnresolved", err)
	}
}

func TestResolveFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(plaintext+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	v, err := secret.Resolve("${file:" + path + "}")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if v.Reveal() != plaintext {
		t.Errorf("Reveal() = %q, want %q (trailing newline must be trimmed)", v.Reveal(), plaintext)
	}
}

func TestResolveFileMissingDoesNotLeakContents(t *testing.T) {
	t.Parallel()

	_, err := secret.Resolve("${file:/nonexistent/wsaw/token}")
	if !errors.Is(err, secret.ErrUnresolved) {
		t.Fatalf("err = %v, want ErrUnresolved", err)
	}
}

func TestResolveEmptyIsNotAnError(t *testing.T) {
	t.Parallel()

	v, err := secret.Resolve("")
	if err != nil {
		t.Fatalf("Resolve(\"\"): %v", err)
	}

	if v.IsSet() {
		t.Error("IsSet() = true for empty reference")
	}
}

// TestValueNeverFormatsPlaintext covers the formatting paths that leak by
// accident: %v, %s, %#v, JSON, and slog.
func TestValueNeverFormatsPlaintext(t *testing.T) {
	t.Parallel()

	v := secret.Literal(plaintext)

	for _, format := range []string{"%v", "%s", "%#v", "%+v"} {
		got := fmt.Sprintf(format, v)
		if strings.Contains(got, plaintext) {
			t.Errorf("fmt.Sprintf(%q, v) leaked plaintext: %s", format, got)
		}

		if got != secret.Redacted {
			t.Errorf("fmt.Sprintf(%q, v) = %q, want %q", format, got, secret.Redacted)
		}
	}

	b, err := json.Marshal(struct{ Token secret.Value }{v})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	if strings.Contains(string(b), plaintext) {
		t.Errorf("JSON leaked plaintext: %s", b)
	}

	var sb strings.Builder

	logger := slog.New(slog.NewTextHandler(&sb, nil))
	logger.Info("auth", "token", v)

	if strings.Contains(sb.String(), plaintext) {
		t.Errorf("slog leaked plaintext: %s", sb.String())
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "userinfo password removed",
			in:   "http://user:pass@proxy.internal:3128",
			want: "http://user:" + secret.Redacted + "@proxy.internal:3128",
		},
		{
			name: "query string removed because webhooks carry tokens",
			in:   "https://hooks.example.com/services/T000/B000?token=abc123",
			want: "https://hooks.example.com/services/T000/B000?" + secret.Redacted,
		},
		{
			name: "clean url unchanged",
			in:   "https://hooks.example.com/path",
			want: "https://hooks.example.com/path",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
		{
			name: "unparseable url is fully redacted rather than echoed",
			in:   "http://[::1]:namedport/x",
			want: secret.Redacted,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := secret.RedactURL(tc.in); got != tc.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRegistryScrub(t *testing.T) {
	t.Parallel()

	var r secret.Registry

	r.Add(secret.Literal(plaintext))
	r.Add(secret.Literal(""))   // ignored
	r.Add(secret.Literal("ab")) // too short to scrub safely

	got := r.Scrub("chromedp: request failed with Authorization: Bearer " + plaintext)
	if strings.Contains(got, plaintext) {
		t.Errorf("Scrub left plaintext: %s", got)
	}

	if !strings.Contains(got, secret.Redacted) {
		t.Errorf("Scrub did not insert marker: %s", got)
	}

	if got := r.Scrub("about ab and abc"); got != "about ab and abc" {
		t.Errorf("short secret should not be scrubbed, got %q", got)
	}
}
