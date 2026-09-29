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
}

// PullReference is what to pass to podman pull or docker pull to fetch the
// newer image, pinned as the configuration should pin it.
func (u ImageUpdate) PullReference() string {
	return u.Checked + "@" + u.Latest
}

// CheckImage asks image's registry which digest it serves for tag and
// compares it with the digest image is pinned by. It pulls nothing: it reads
// one manifest, anonymously, and sends no credentials of any kind.
func CheckImage(ctx context.Context, client *http.Client, userAgent, image, tag string) (ImageUpdate, error) {
	ref, err := ParseReference(image)
	if err != nil {
		return ImageUpdate{}, err
	}

	if ref.Digest == "" {
		return ImageUpdate{}, fmt.Errorf("%s: %w", image, ErrNotPinned)
	}

	r := registryClient{http: client, userAgent: userAgent}

	body, err := r.manifest(ctx, ref, tag)
	if err != nil {
		return ImageUpdate{}, fmt.Errorf("reading %s:%s from %s: %w", ref.Repository, tag, ref.Registry, err)
	}

	// The digest is taken from the bytes rather than from the
	// Docker-Content-Digest header, which not every registry sends and which
	// would otherwise be trusted without being verified.
	sum := sha256.Sum256(body)
	latest := "sha256:" + hex.EncodeToString(sum[:])

	name, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name = name[:i]
	}

	return ImageUpdate{
		Checked: name + ":" + tag,
		Latest:  latest,
		Current: latest == ref.Digest || listsManifest(body, ref.Digest),
	}, nil
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

type registryClient struct {
	http      *http.Client
	userAgent string
}

// manifest fetches the manifest for tag, answering the registry's challenge
// for an anonymous pull token when it sends one. Public images on ghcr.io and
// Docker Hub both require that token even to be read.
func (r registryClient) manifest(ctx context.Context, ref Reference, tag string) ([]byte, error) {
	u := (&url.URL{
		Scheme: "https",
		Host:   ref.Registry,
		Path:   "/v2/" + ref.Repository + "/manifests/" + tag,
	}).String()

	resp, err := r.get(ctx, u, manifestAccept, "")
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		drain(resp)

		token, err := r.token(ctx, challenge)
		if err != nil {
			return nil, err
		}

		resp, err = r.get(ctx, u, manifestAccept, token)
		if err != nil {
			return nil, err
		}
	}

	defer drain(resp)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry answered %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}

	if len(body) > maxManifestBytes {
		return nil, fmt.Errorf("manifest larger than %d bytes", maxManifestBytes)
	}

	return body, nil
}

// token fetches an anonymous bearer token from the realm a challenge names.
// The realm is the registry's to choose, so it is held to https: a plain-http
// realm would be a downgrade the registry's own TLS did not agree to.
func (r registryClient) token(ctx context.Context, challenge string) (string, error) {
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

func (r registryClient) get(ctx context.Context, u, accept, token string) (*http.Response, error) {
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
