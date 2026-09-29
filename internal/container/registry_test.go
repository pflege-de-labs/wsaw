package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want Reference
	}{
		{
			in:   "ghcr.io/pflege-de-labs/wsaw-browser@sha256:abc",
			want: Reference{Registry: "ghcr.io", Repository: "pflege-de-labs/wsaw-browser", Digest: "sha256:abc"},
		},
		{
			in:   "ghcr.io/pflege-de-labs/wsaw-browser:0.3@sha256:abc",
			want: Reference{Registry: "ghcr.io", Repository: "pflege-de-labs/wsaw-browser", Tag: "0.3", Digest: "sha256:abc"},
		},
		{
			in:   "docker.io/chromedp/headless-shell@sha256:abc",
			want: Reference{Registry: "registry-1.docker.io", Repository: "chromedp/headless-shell", Digest: "sha256:abc"},
		},
		{
			in:   "chromedp/headless-shell:latest",
			want: Reference{Registry: "registry-1.docker.io", Repository: "chromedp/headless-shell", Tag: "latest"},
		},
		{
			in:   "alpine",
			want: Reference{Registry: "registry-1.docker.io", Repository: "library/alpine"},
		},
		{
			in:   "localhost:5000/browser:1@sha256:abc",
			want: Reference{Registry: "localhost:5000", Repository: "browser", Tag: "1", Digest: "sha256:abc"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := ParseReference(tt.in)
			if err != nil {
				t.Fatalf("ParseReference(%q): %v", tt.in, err)
			}

			if got != tt.want {
				t.Errorf("ParseReference(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseReferenceRejectsMalformed(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"ghcr.io/", "ghcr.io/a b"} {
		if _, err := ParseReference(in); err == nil {
			t.Errorf("ParseReference(%q) succeeded, want an error", in)
		}
	}
}

// fakeRegistry serves one manifest for one tag behind the anonymous-token
// challenge ghcr.io and Docker Hub use.
type fakeRegistry struct {
	manifest []byte
	realm    string // overrides the token realm when set
}

const fakeToken = "anon-token"

func (f *fakeRegistry) start(t *testing.T) (*httptest.Server, string) {
	t.Helper()

	mux := http.NewServeMux()

	var srv *httptest.Server

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:org/browser:pull" {
			http.Error(w, "bad scope", http.StatusBadRequest)

			return
		}

		_, _ = w.Write([]byte(`{"token":"` + fakeToken + `"}`))
	})
	mux.HandleFunc("/v2/org/browser/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			realm := f.realm
			if realm == "" {
				realm = srv.URL + "/token"
			}

			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+realm+`",service="fake",scope="repository:org/browser:pull"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			http.Error(w, "index not accepted", http.StatusNotAcceptable)

			return
		}

		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		_, _ = w.Write(f.manifest)
	})

	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	return srv, srv.Listener.Addr().String() + "/org/browser"
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)

	return "sha256:" + hex.EncodeToString(sum[:])
}

const platformDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

var fakeIndex = []byte(`{"schemaVersion":2,"manifests":[{"digest":"` + platformDigest + `","platform":{"architecture":"arm64","os":"linux"}}]}`)

func TestCheckImage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		pinned      string
		wantCurrent bool
	}{
		{name: "pinned to the latest index", pinned: digestOf(fakeIndex), wantCurrent: true},
		{name: "pinned to a platform manifest of the latest index", pinned: platformDigest, wantCurrent: true},
		{name: "pinned to an older image", pinned: "sha256:" + strings.Repeat("0", 64), wantCurrent: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, repo := (&fakeRegistry{manifest: fakeIndex}).start(t)

			got, err := CheckImage(context.Background(), srv.Client(), "wsaw-test", repo+":0.3@"+tt.pinned, LatestTag)
			if err != nil {
				t.Fatalf("CheckImage: %v", err)
			}

			if got.Current != tt.wantCurrent {
				t.Errorf("Current = %v, want %v", got.Current, tt.wantCurrent)
			}

			if got.Latest != digestOf(fakeIndex) {
				t.Errorf("Latest = %s, want %s", got.Latest, digestOf(fakeIndex))
			}

			if want := repo + ":latest@" + digestOf(fakeIndex); got.PullReference() != want {
				t.Errorf("PullReference() = %s, want %s", got.PullReference(), want)
			}
		})
	}
}

func TestCheckImageNotPinned(t *testing.T) {
	t.Parallel()

	_, err := CheckImage(context.Background(), http.DefaultClient, "wsaw-test", "ghcr.io/org/browser:latest", LatestTag)
	if !errors.Is(err, ErrNotPinned) {
		t.Fatalf("CheckImage of a floating tag: err = %v, want ErrNotPinned", err)
	}
}

// A registry that names a plain-http token realm is refused rather than
// followed, so the check never downgrades from the registry's own TLS.
func TestCheckImageRefusesInsecureRealm(t *testing.T) {
	t.Parallel()

	srv, repo := (&fakeRegistry{manifest: fakeIndex, realm: "http://127.0.0.1:1/token"}).start(t)

	_, err := CheckImage(context.Background(), srv.Client(), "wsaw-test", repo+"@"+platformDigest, LatestTag)
	if err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Fatalf("CheckImage with an http realm: err = %v, want a refusal", err)
	}
}

func TestCheckImageMissingTag(t *testing.T) {
	t.Parallel()

	srv, repo := (&fakeRegistry{manifest: fakeIndex}).start(t)

	_, err := CheckImage(context.Background(), srv.Client(), "wsaw-test", repo+"@"+platformDigest, "no-such-tag")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("CheckImage of a missing tag: err = %v, want a 404", err)
	}
}

func TestParseBearerChallenge(t *testing.T) {
	t.Parallel()

	got, ok := parseBearerChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:a/b:pull"`)
	if !ok {
		t.Fatal("parseBearerChallenge rejected a well-formed challenge")
	}

	want := map[string]string{"realm": "https://ghcr.io/token", "service": "ghcr.io", "scope": "repository:a/b:pull"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	for _, bad := range []string{"", `Basic realm="x"`, `Bearer service="x"`, `Bearer realm="unterminated`} {
		if _, ok := parseBearerChallenge(bad); ok {
			t.Errorf("parseBearerChallenge(%q) accepted a malformed challenge", bad)
		}
	}
}
