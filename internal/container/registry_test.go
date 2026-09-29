package container

import (
	"context"
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

// fakeRegistry serves one manifest for the latest tag, and objects by
// digest, behind the anonymous-token challenge ghcr.io and Docker Hub use.
type fakeRegistry struct {
	manifest []byte
	objects  map[string][]byte // by digest; served as manifests and blobs
	realm    string            // overrides the token realm when set
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
	mux.HandleFunc("/v2/org/browser/", func(w http.ResponseWriter, r *http.Request) {
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

		rest := strings.TrimPrefix(r.URL.Path, "/v2/org/browser/")

		if rest == "manifests/latest" {
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
				http.Error(w, "index not accepted", http.StatusNotAcceptable)

				return
			}

			_, _ = w.Write(f.manifest)

			return
		}

		_, digest, _ := strings.Cut(rest, "/")
		if body, ok := f.objects[digest]; ok {
			_, _ = w.Write(body)

			return
		}

		http.NotFound(w, r)
	})

	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	return srv, srv.Listener.Addr().String() + "/org/browser"
}

// fakeImage is a published multi-platform image whose configuration
// declares chromium as its LabelChromiumVersion.
type fakeImage struct {
	index, platform, config []byte
}

func newFakeImage(chromium string) fakeImage {
	config := []byte(`{"architecture":"arm64","os":"linux","config":{"Labels":{"` +
		LabelChromiumVersion + `":"` + chromium + `","org.opencontainers.image.version":"0.3.0"}}}`)
	platform := []byte(`{"schemaVersion":2,"config":{"digest":"` + digestOf(config) + `"},"layers":[]}`)
	index := []byte(`{"schemaVersion":2,"manifests":[` +
		`{"digest":"sha256:` + strings.Repeat("9", 64) + `","platform":{"architecture":"unknown","os":"unknown"}},` +
		`{"digest":"` + digestOf(platform) + `","platform":{"architecture":"arm64","os":"linux"}}]}`)

	return fakeImage{index: index, platform: platform, config: config}
}

func (i fakeImage) registry() *fakeRegistry {
	return &fakeRegistry{manifest: i.index, objects: map[string][]byte{
		digestOf(i.platform): i.platform,
		digestOf(i.config):   i.config,
	}}
}

var (
	latestImage    = newFakeImage("152.0.7977.82-r0")
	fakeIndex      = latestImage.index
	platformDigest = digestOf(latestImage.platform)
	olderDigest    = "sha256:" + strings.Repeat("0", 64)
)

func TestCheckImage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		pinned       string
		wantCurrent  bool
		wantChromium string
	}{
		{name: "pinned to the latest index", pinned: digestOf(fakeIndex), wantCurrent: true},
		{name: "pinned to a platform manifest of the latest index", pinned: platformDigest, wantCurrent: true},
		{name: "pinned to an older image", pinned: olderDigest, wantChromium: "152.0.7977.82-r0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, repo := latestImage.registry().start(t)

			got, err := CheckImage(context.Background(), srv.Client(), "wsaw-test", repo+":0.3@"+tt.pinned, LatestTag)
			if err != nil {
				t.Fatalf("CheckImage: %v", err)
			}

			if got.Current != tt.wantCurrent {
				t.Errorf("Current = %v, want %v", got.Current, tt.wantCurrent)
			}

			if got.LatestChromium != tt.wantChromium {
				t.Errorf("LatestChromium = %q, want %q", got.LatestChromium, tt.wantChromium)
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

// A label that cannot be trusted or reached leaves LatestChromium empty, so
// the digests decide and the check still reports the image as superseded.
func TestCheckImageUnreadableLabel(t *testing.T) {
	t.Parallel()

	tampered := latestImage.registry()
	tampered.objects[digestOf(latestImage.config)] = []byte(`{"config":{"Labels":{"` + LabelChromiumVersion + `":"999.0.0.0-r0"}}}`)

	nested := latestImage.registry()
	nestedIndex := []byte(`{"schemaVersion":2,"manifests":[{"digest":"` + digestOf(fakeIndex) + `","platform":{"os":"linux"}}]}`)
	nested.manifest = nestedIndex
	nested.objects[digestOf(fakeIndex)] = fakeIndex

	missing := latestImage.registry()
	delete(missing.objects, digestOf(latestImage.config))

	for name, reg := range map[string]*fakeRegistry{
		"config blob does not match its digest": tampered,
		"platform manifest is another index":    nested,
		"config blob missing":                   missing,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv, repo := reg.start(t)

			got, err := CheckImage(context.Background(), srv.Client(), "wsaw-test", repo+"@"+olderDigest, LatestTag)
			if err != nil {
				t.Fatalf("CheckImage: %v", err)
			}

			if got.Current || got.LatestChromium != "" {
				t.Errorf("got Current=%v LatestChromium=%q, want a superseded image with no label", got.Current, got.LatestChromium)
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
