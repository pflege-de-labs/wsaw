package container

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// LatestTag is the tag an update check compares a pinned image with. The
// release workflow moves it with every release that is not a pre-release, so
// it names what an operator following releases would run.
const LatestTag = "latest"

// ErrNotPinned is returned by CheckImage for an image without a digest. A
// floating tag already follows the registry, so there is nothing to compare.
var ErrNotPinned = errors.New("image is not pinned by digest")

// Manifest bodies are a few kilobytes; the caps only stop a misbehaving
// registry from streaming without end into memory.
const (
	maxManifestBytes = 4 << 20
	maxTokenBytes    = 64 << 10
)

// Accepted manifest media types, index types first: a multi-architecture
// image is pinned by the digest of its index, and that is what the registry
// must return for the two digests to be comparable.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Reference is an image reference split into the parts a registry request
// needs.
type Reference struct {
	// Registry is the host that serves the image's API, which for Docker Hub
	// is not the docker.io the reference names.
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

// ParseReference splits an image reference such as
// ghcr.io/org/image:1.2@sha256:... into its parts, applying the Docker Hub
// conventions for a reference without a registry host.
func ParseReference(image string) (Reference, error) {
	var ref Reference

	rest := image
	if name, digest, ok := strings.Cut(rest, "@"); ok {
		rest, ref.Digest = name, digest
	}

	// A colon after the last slash is a tag; one before it is a port.
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		rest, ref.Tag = rest[:i], rest[i+1:]
	}

	host, repo, ok := strings.Cut(rest, "/")
	if !ok || !strings.ContainsAny(host, ".:") && host != "localhost" {
		host, repo = "docker.io", rest
	}

	if host == "docker.io" {
		host = "registry-1.docker.io"

		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}

	if repo == "" || strings.ContainsAny(repo, " ?#") {
		return Reference{}, fmt.Errorf("image reference %q: malformed repository", image)
	}

	ref.Registry, ref.Repository = host, repo

	return ref, nil
}

// ImageUpdate is the outcome of comparing a pinned image with the digest its
// registry currently serves for a tag.
type ImageUpdate struct {
	// Checked names what was compared with, as repository:tag.
	Checked string
	// Latest is the digest the registry serves for the tag.
	Latest string
	// Current is true when the pinned digest is Latest, or is one of the
	// platform manifests Latest lists — an image pinned by the digest of its
	// own architecture rather than by the index.
	Current bool
	// LatestChromium is the latest image's LabelChromiumVersion when it is not
	// Current and declares one, and empty otherwise — including when the
	// label could not be read, which leaves the digests to decide.
	LatestChromium string
}

// PullReference is what to pass to podman pull or docker pull to fetch the
// newer image, pinned as the configuration should pin it.
func (u ImageUpdate) PullReference() string {
	return u.Checked + "@" + u.Latest
}

// CheckImage asks image's registry which digest it serves for tag and
// compares it with the digest image is pinned by. When they differ it also
// reads the latest image's configuration for LabelChromiumVersion. It pulls
// nothing — no layer is fetched — reads anonymously, and sends no credentials
// of any kind.
func CheckImage(ctx context.Context, client *http.Client, userAgent, image, tag string) (ImageUpdate, error) {
	ref, err := ParseReference(image)
	if err != nil {
		return ImageUpdate{}, err
	}

	if ref.Digest == "" {
		return ImageUpdate{}, fmt.Errorf("%s: %w", image, ErrNotPinned)
	}

	r := &registryClient{http: client, userAgent: userAgent, ref: ref}

	body, err := r.fetch(ctx, "manifests/"+tag, manifestAccept)
	if err != nil {
		return ImageUpdate{}, fmt.Errorf("reading %s:%s from %s: %w", ref.Repository, tag, ref.Registry, err)
	}

	// The digest is taken from the bytes rather than from the
	// Docker-Content-Digest header, which not every registry sends and which
	// would otherwise be trusted without being verified.
	latest := digestOf(body)

	name, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}

	update := ImageUpdate{
		Checked: name + ":" + tag,
		Latest:  latest,
		Current: latest == ref.Digest || listsManifest(body, ref.Digest),
	}

	if !update.Current {
		// Unreadable labels are not an error: the digests have already
		// answered, and the label only lets a rebuild of the same browser
		// read as current.
		update.LatestChromium, _ = r.chromiumLabel(ctx, body, true)
	}

	return update, nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)

	return "sha256:" + hex.EncodeToString(sum[:])
}

// chromiumLabel reads LabelChromiumVersion from the configuration of the
// image a manifest describes. For an index it reads the first Linux platform
// manifest's: every platform is built from one Dockerfile with one pinned
// Chromium, and the registry refuses a push where they would not agree.
func (r *registryClient) chromiumLabel(ctx context.Context, manifest []byte, allowIndex bool) (string, error) {
	var m struct {
		Config    descriptor   `json:"config"`
		Manifests []descriptor `json:"manifests"`
	}

	if err := json.Unmarshal(manifest, &m); err != nil {
		return "", fmt.Errorf("decoding manifest: %w", err)
	}

	if m.Config.Digest == "" {
		if !allowIndex {
			return "", errors.New("platform manifest names no configuration")
		}

		for _, p := range m.Manifests {
			if p.Platform.OS != "linux" {
				continue
			}

			platform, err := r.fetchVerified(ctx, "manifests/", p.Digest, manifestAccept)
			if err != nil {
				return "", err
			}

			// A platform manifest names a configuration; one that is another
			// index instead is refused rather than followed.
			return r.chromiumLabel(ctx, platform, false)
		}

		return "", errors.New("image lists no linux platform")
	}

	blob, err := r.fetchVerified(ctx, "blobs/", m.Config.Digest, "application/json")
	if err != nil {
		return "", err
	}

	var cfg struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"config"`
	}

	if err := json.Unmarshal(blob, &cfg); err != nil {
		return "", fmt.Errorf("decoding image configuration: %w", err)
	}

	return cfg.Config.Labels[LabelChromiumVersion], nil
}

// descriptor is the part of an OCI content descriptor the check reads.
type descriptor struct {
	Digest   string `json:"digest"`
	Platform struct {
		OS string `json:"os"`
	} `json:"platform"`
}

// fetchVerified fetches content by digest and refuses it unless it hashes to
// that digest, so a registry cannot answer for one object with another.
func (r *registryClient) fetchVerified(ctx context.Context, kind, digest, accept string) ([]byte, error) {
	if !strings.HasPrefix(digest, "sha256:") || strings.ContainsAny(digest, "/?#") {
		return nil, fmt.Errorf("unsupported digest %q", digest)
	}

	body, err := r.fetch(ctx, kind+digest, accept)
	if err != nil {
		return nil, err
	}

	if got := digestOf(body); got != digest {
		return nil, fmt.Errorf("registry served %s for %s", got, digest)
	}

	return body, nil
}

// listsManifest reports whether an image index names digest among its
// platform manifests. A body that is not an index lists nothing.
func listsManifest(body []byte, digest string) bool {
	var index struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}

	if json.Unmarshal(body, &index) != nil {
		return false
	}

	for _, m := range index.Manifests {
		if m.Digest == digest {
			return true
		}
	}

	return false
}

// registryClient reads from one repository of one registry.
type registryClient struct {
	http      *http.Client
	userAgent string
	ref       Reference
	// token is the anonymous pull token, once a challenge has asked for one.
	token string
}

// fetch reads /v2/<repository>/<path>, answering the registry's challenge
// for an anonymous pull token when it sends one. Public images on ghcr.io and
// Docker Hub both require that token even to be read. A blob may be served
// by a redirect to another host; net/http does not carry the token there.
func (r *registryClient) fetch(ctx context.Context, path, accept string) ([]byte, error) {
	u := (&url.URL{
		Scheme: "https",
		Host:   r.ref.Registry,
		Path:   "/v2/" + r.ref.Repository + "/" + path,
	}).String()

	resp, err := r.get(ctx, u, accept, r.token)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized && r.token == "" {
		challenge := resp.Header.Get("WWW-Authenticate")
		drain(resp)

		if r.token, err = r.fetchToken(ctx, challenge); err != nil {
			return nil, err
		}

		if resp, err = r.get(ctx, u, accept, r.token); err != nil {
			return nil, err
		}
	}

	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry answered %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	if len(body) > maxManifestBytes {
		return nil, fmt.Errorf("%s larger than %d bytes", path, maxManifestBytes)
	}

	return body, nil
}

// fetchToken fetches an anonymous bearer token from the realm a challenge names.
// The realm is the registry's to choose, so it is held to https: a plain-http
// realm would be a downgrade the registry's own TLS did not agree to.
func (r *registryClient) fetchToken(ctx context.Context, challenge string) (string, error) {
	params, ok := parseBearerChallenge(challenge)
	if !ok {
		return "", fmt.Errorf("registry requires authentication this check does not offer: %q", challenge)
	}

	realm, err := url.Parse(params["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return "", fmt.Errorf("registry token realm %q is not an https URL", params["realm"])
	}

	q := realm.Query()

	for _, k := range []string{"service", "scope"} {
		if v := params[k]; v != "" {
			q.Set(k, v)
		}
	}

	realm.RawQuery = q.Encode()

	resp, err := r.get(ctx, realm.String(), "application/json", "")
	if err != nil {
		return "", err
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token endpoint answered %s", resp.Status)
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenBytes)).Decode(&body); err != nil {
		return "", fmt.Errorf("decoding registry token: %w", err)
	}

	if body.Token != "" {
		return body.Token, nil
	}

	if body.AccessToken != "" {
		return body.AccessToken, nil
	}

	return "", errors.New("registry token endpoint returned no token")
}

func (r *registryClient) get(ctx context.Context, u, accept, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", r.userAgent)

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", req.URL.Redacted(), err)
	}

	return resp, nil
}

// drain reads what is left of a response so its connection can be reused,
// and closes it. Nothing read here is needed, and a failure to close a body
// that has been read leaves nothing to recover.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxManifestBytes))
	_ = resp.Body.Close()
}

// parseBearerChallenge reads the parameters of a WWW-Authenticate header of
// the Bearer scheme, e.g. Bearer realm="https://ghcr.io/token",service="ghcr.io".
func parseBearerChallenge(h string) (map[string]string, bool) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}

	params := map[string]string{}

	for rest = strings.TrimSpace(rest); rest != ""; {
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			return nil, false
		}

		key = strings.ToLower(strings.TrimSpace(key))

		var value string

		if strings.HasPrefix(after, `"`) {
			end := strings.Index(after[1:], `"`)
			if end < 0 {
				return nil, false
			}

			value, rest = after[1:end+1], after[end+2:]
		} else {
			value, rest, _ = strings.Cut(after, ",")
			rest = "," + rest
		}

		params[key] = value
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ","))
	}

	return params, params["realm"] != ""
}
