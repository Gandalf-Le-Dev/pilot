// Package registry reads tag lists from OCI container registries.
//
// Only the one endpoint Pilot needs — `/v2/<repo>/tags/list` — with the token
// dance that ghcr.io and Docker Hub both require. They speak the same protocol,
// so one client covers every registry the fleet pulls from.
//
// Nothing here pulls, pushes or authenticates as a user. It reads what tags
// exist, which is public information for a public image and a 401 for a private
// one — and a 401 is reported as such rather than as "no updates", because a
// checker that goes quiet when it loses access is worse than no checker.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultRegistry is where an image with no registry host comes from.
const DefaultRegistry = "docker.io"

// dockerAPI is Docker Hub's registry endpoint. The `docker.io` in an image
// reference is a name, not a host that serves the API.
const dockerAPI = "registry-1.docker.io"

// Ref is a parsed image reference.
type Ref struct {
	Registry string // "ghcr.io", "docker.io"
	Repo     string // "paperless-ngx/paperless-ngx", "library/postgres"
	Tag      string // "3.0.4"
}

// Name is the image without its tag, written the way a person would.
//
// The registry host is kept unless it is Docker Hub, whose name is
// conventionally implicit — and its `library/` namespace is dropped for the
// same reason. Dropping the host for *every* registry, which this did at first,
// renders `ghcr.io/atuinsh/atuin` as `atuinsh/atuin`: indistinguishable from a
// Docker Hub image of the same name, which is a different image entirely.
func (r Ref) Name() string {
	if r.Registry == DefaultRegistry {
		return strings.TrimPrefix(r.Repo, "library/")
	}
	return r.Registry + "/" + r.Repo
}

// String rebuilds the full reference.
func (r Ref) String() string { return r.Name() + ":" + r.Tag }

// hostish matches a first path component that names a registry rather than a
// user: it has a dot, a port, or is localhost.
var hostish = regexp.MustCompile(`^(localhost|[^/]+[.:][^/]*)$`)

// ParseRef reads an image reference.
//
// Digest-pinned references are rejected: a digest names exact bits with no
// series to compare against, which is the whole point of pinning one.
func ParseRef(image string) (Ref, error) {
	if strings.Contains(image, "@") {
		return Ref{}, fmt.Errorf("%q is pinned to a digest, which names no version series", image)
	}

	name, tag := image, "latest"
	// Split on the last colon, but only if it is in the final path segment —
	// otherwise a registry port (`localhost:5000/x`) reads as a tag.
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		name, tag = image[:i], image[i+1:]
	}
	if name == "" {
		return Ref{}, fmt.Errorf("%q has no image name", image)
	}

	reg := DefaultRegistry
	repo := name
	if first, rest, ok := strings.Cut(name, "/"); ok && hostish.MatchString(first) {
		reg, repo = first, rest
	}
	// Docker Hub's official images live under an implicit `library/`.
	if reg == DefaultRegistry && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return Ref{Registry: reg, Repo: repo, Tag: tag}, nil
}

// Client reads tag lists.
type Client struct {
	HTTP *http.Client
}

func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// ErrUnauthorized means the registry would not show us this repository.
var ErrUnauthorized = fmt.Errorf("registry denied access")

// Tags lists every tag for a repository.
//
// Results are paginated by Link header. Following it matters: a repository with
// more tags than one page would otherwise report only the first slice, and
// since registries do not order tags, that slice is arbitrary rather than
// oldest — so the newest release could simply be absent.
func (c *Client) Tags(ctx context.Context, ref Ref) ([]string, error) {
	token, err := c.token(ctx, ref)
	if err != nil {
		return nil, err
	}

	host := ref.Registry
	if host == DefaultRegistry {
		host = dockerAPI
	}
	next := fmt.Sprintf("https://%s/v2/%s/tags/list?n=1000", host, ref.Repo)

	var all []string
	for range 20 { // a hard stop; 20k tags is already far past reasonable
		page, link, err := c.page(ctx, next, token)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if link == "" {
			return all, nil
		}
		next = "https://" + host + link
	}
	return all, nil
}

func (c *Client) page(ctx context.Context, url, token string) (tags []string, next string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, "", ErrUnauthorized
	case http.StatusTooManyRequests:
		return nil, "", fmt.Errorf("registry rate limit reached")
	default:
		return nil, "", fmt.Errorf("registry returned %s", res.Status)
	}

	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("reading tag list: %w", err)
	}
	return body.Tags, nextLink(res.Header.Get("Link")), nil
}

// nextLink extracts the path from a `Link: </v2/...>; rel="next"` header.
func nextLink(h string) string {
	if h == "" || !strings.Contains(h, `rel="next"`) {
		return ""
	}
	start := strings.Index(h, "<")
	end := strings.Index(h, ">")
	if start < 0 || end < start {
		return ""
	}
	return h[start+1 : end]
}

// token fetches a pull token.
//
// Anonymous, because listing tags on a public repository needs no identity.
// A private repository answers 401 here, and that is reported rather than
// swallowed.
func (c *Client) token(ctx context.Context, ref Ref) (string, error) {
	var endpoint, service string
	switch ref.Registry {
	case DefaultRegistry:
		endpoint, service = "https://auth.docker.io/token", "registry.docker.io"
	default:
		endpoint, service = "https://"+ref.Registry+"/token", ref.Registry
	}

	q := url.Values{
		"service": {service},
		"scope":   {"repository:" + ref.Repo + ":pull"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}

	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		// Some registries need no token at all. Carrying on unauthenticated is
		// right here: the tag request will report its own 401 if one is needed,
		// with a clearer message than this would give.
		return "", nil
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", nil
	}
	if body.Token != "" {
		return body.Token, nil
	}
	return body.AccessToken, nil
}
