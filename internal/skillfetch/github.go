package skillfetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	MaxDownloadBytes int64 = 16 << 20
	DefaultTimeout         = 30 * time.Second
	UserAgent              = "Karoz-OSS-SkillFetch/1.0"
)

var (
	validOwnerRepoSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	validRefSegment       = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,128}$`)
	ErrInvalidSource      = errors.New("invalid GitHub skill source")
	ErrRedirect           = errors.New("disallowed redirect host")
)

type RepoSpec struct {
	Owner   string
	Repo    string
	Ref     string
	Subpath string
}

// ParseGitHubSource parses:
// - owner/repo
// - owner/repo/subpath/to/skill
// - https://github.com/owner/repo
// - https://github.com/owner/repo/tree/<ref>/<subpath>
func ParseGitHubSource(raw string) (RepoSpec, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.ContainsAny(trimmed, "\x00\r\n") {
		return RepoSpec{}, fmt.Errorf("%w: empty source", ErrInvalidSource)
	}
	trimmed = strings.TrimSuffix(trimmed, ".git")

	if strings.HasPrefix(trimmed, "https://") || strings.HasPrefix(trimmed, "http://") {
		u, err := url.Parse(trimmed)
		if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil {
			return RepoSpec{}, fmt.Errorf("%w: only https://github.com URLs or owner/repo shorthand are supported", ErrInvalidSource)
		}
		parts := splitCleanPath(u.Path)
		if len(parts) < 2 || hasDotSegment(parts) {
			return RepoSpec{}, fmt.Errorf("%w: missing owner or repository or unsafe path segment", ErrInvalidSource)
		}
		spec := RepoSpec{Owner: parts[0], Repo: parts[1]}
		if len(parts) > 2 {
			switch parts[2] {
			case "tree", "blob":
				if len(parts) < 4 {
					return RepoSpec{}, fmt.Errorf("%w: missing branch or ref after /%s/", ErrInvalidSource, parts[2])
				}
				spec.Ref = parts[3]
				if len(parts) > 4 {
					spec.Subpath = path.Clean(strings.Join(parts[4:], "/"))
				}
			default:
				spec.Subpath = path.Clean(strings.Join(parts[2:], "/"))
			}
		}
		if err := validateRepoSpec(spec); err != nil {
			return RepoSpec{}, err
		}
		return spec, nil
	}

	parts := splitCleanPath(trimmed)
	if len(parts) < 2 || hasDotSegment(parts) {
		return RepoSpec{}, fmt.Errorf("%w: use owner/repo or https://github.com/owner/repo", ErrInvalidSource)
	}
	spec := RepoSpec{Owner: parts[0], Repo: parts[1]}
	if len(parts) > 2 {
		spec.Subpath = path.Clean(strings.Join(parts[2:], "/"))
	}
	if err := validateRepoSpec(spec); err != nil {
		return RepoSpec{}, err
	}
	return spec, nil
}

func splitCleanPath(raw string) []string {
	var out []string
	for _, seg := range strings.Split(strings.Trim(raw, "/"), "/") {
		seg = strings.TrimSpace(seg)
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

func hasDotSegment(parts []string) bool {
	for _, seg := range parts {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func validateRepoSpec(spec RepoSpec) error {
	if !validOwnerRepoSegment.MatchString(spec.Owner) || !validOwnerRepoSegment.MatchString(spec.Repo) || spec.Owner == ".." || spec.Repo == ".." {
		return fmt.Errorf("%w: invalid owner or repository name", ErrInvalidSource)
	}
	if spec.Ref != "" && (!validRefSegment.MatchString(spec.Ref) || strings.Contains(spec.Ref, "..")) {
		return fmt.Errorf("%w: invalid git ref", ErrInvalidSource)
	}
	if spec.Subpath != "" {
		if spec.Subpath == "." || spec.Subpath == ".." || strings.HasPrefix(spec.Subpath, "../") || strings.Contains(spec.Subpath, "..") {
			return fmt.Errorf("%w: invalid subpath", ErrInvalidSource)
		}
	}
	return nil
}

// TarballURL returns the GitHub tarball download endpoint for spec.
func (s RepoSpec) TarballURL() string {
	if s.Ref != "" {
		return fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/%s", url.PathEscape(s.Owner), url.PathEscape(s.Repo), url.PathEscape(s.Ref))
	}
	return fmt.Sprintf("https://api.github.com/repos/%s/%s/tarball", url.PathEscape(s.Owner), url.PathEscape(s.Repo))
}

// Install fetches the GitHub tarball archive over HTTPS (or a custom test URL via client)
// and safely extracts the validated skill into destRoot/<slug>.
func Install(ctx context.Context, client *http.Client, destRoot, rawSource string) (InstalledSkill, error) {
	spec, err := ParseGitHubSource(rawSource)
	if err != nil {
		return InstalledSkill{}, err
	}
	return InstallFromURL(ctx, client, destRoot, spec, spec.TarballURL())
}

func InstallFromURL(ctx context.Context, client *http.Client, destRoot string, spec RepoSpec, rawURL string) (InstalledSkill, error) {
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}

	// Copy client settings to enforce safe host-restricted redirects without mutating caller client.
	safeClient := *client
	safeClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return ErrRedirect
		}
		if req.URL.Scheme != "https" || !isAllowedArchiveHost(req.URL.Host) {
			return ErrRedirect
		}
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return InstalledSkill{}, err
	}
	req.Header.Set("User-Agent", UserAgent)

	resp, err := safeClient.Do(req)
	if err != nil {
		return InstalledSkill{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return InstalledSkill{}, fmt.Errorf("github tarball HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > MaxDownloadBytes {
		return InstalledSkill{}, fmt.Errorf("archive exceeds %d bytes", MaxDownloadBytes)
	}
	return InstallFromTarGz(resp.Body, destRoot, spec)
}

func isAllowedArchiveHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == "codeload.github.com" || h == "api.github.com" || strings.HasPrefix(h, "127.0.0.1:") || strings.HasPrefix(h, "localhost:")
}
