package ghapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
)

// RepoTag is a tag name paired with the commit SHA it resolves to, as
// returned by the repos/tags endpoint (annotated tags dereferenced).
type RepoTag struct {
	Name string
	SHA  string
}

// RepoTags lists a repository's tags (up to 100) with the commit SHA each
// resolves to. Delegates to ListTags so it shares the cached, singleflight-
// coalesced tag fetch rather than issuing a second identical request.
func (c *Client) RepoTags(ctx context.Context, owner, repo string) ([]RepoTag, error) {
	entries, err := c.ListTags(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("fetching tags for %s/%s: %w", owner, repo, err)
	}
	out := make([]RepoTag, 0, len(entries))
	for _, t := range entries {
		out = append(out, RepoTag{Name: t.Name, SHA: t.SHA})
	}
	return out, nil
}

// RepoRelease holds release metadata for a tag.
type RepoRelease struct {
	TagName     string
	PublishedAt string // ISO 8601 date
	Immutable   bool
}

// Releases lists a repository's most recent releases (up to 30).
func (c *Client) Releases(ctx context.Context, owner, repo string) ([]RepoRelease, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("repos/%s/%s/releases?per_page=30",
		url.PathEscape(owner), url.PathEscape(repo))

	var releases []struct {
		TagName     string `json:"tag_name"`
		PublishedAt string `json:"published_at"`
		Immutable   bool   `json:"immutable"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &releases); err != nil {
		if IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
			if anonErr := c.anonGet(ctx, path, &releases); anonErr != nil {
				return nil, fmt.Errorf("anonymous fallback fetching releases for %s/%s: %w", owner, repo, anonErr)
			}
		} else {
			return nil, fmt.Errorf("fetching releases for %s/%s: %w", owner, repo, c.ssoErr(owner, err))
		}
	}

	out := make([]RepoRelease, 0, len(releases))
	for _, rel := range releases {
		out = append(out, RepoRelease(rel))
	}
	return out, nil
}

// RepoMetadata holds repository-level metadata relevant for pinning decisions.
type RepoMetadata struct {
	// FullName is the canonical OWNER/REPO, which differs from the requested
	// name after a rename or transfer.
	FullName      string
	DefaultBranch string
	Visibility    string // "public", "private", or "internal"
	PushedAt      string // ISO 8601 timestamp of last push
}

// RepoMetadata fetches a repository's default branch, visibility, and last
// push time. Delegates to the shared repoMetadata fetch so it shares the
// cached, singleflight-coalesced repos/{owner}/{repo} round-trip with
// GetDefaultBranch and RepoIDs.
func (c *Client) RepoMetadata(ctx context.Context, owner, repo string) (RepoMetadata, error) {
	m, err := c.repoMetadata(ctx, owner, repo)
	if err != nil {
		return RepoMetadata{}, fmt.Errorf("fetching metadata for %s/%s: %w", owner, repo, err)
	}
	return RepoMetadata{
		FullName:      m.FullName,
		DefaultBranch: m.DefaultBranch,
		Visibility:    m.Visibility,
		PushedAt:      m.PushedAt,
	}, nil
}

// ResolvePinRef returns the commit ref names. A name is looked up only as
// refs/tags/<name> then refs/heads/<name>; when both exist at different
// commits it returns *AmbiguousRefError, and when neither exists
// *RefNotFoundError.
func (c *Client) ResolvePinRef(ctx context.Context, owner, repo string, ref parserlock.PinRef) (string, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	if commit, ok := ref.Commit(); ok {
		sha, found, err := c.commitSHA(ctx, owner, repo, commit.String())
		if err != nil {
			return "", err
		}
		if !found {
			return "", &RefNotFoundError{Owner: owner, Repo: repo, Ref: commit.String(),
				Err: fmt.Errorf("commit %s does not exist in %s/%s", commit, owner, repo)}
		}
		return sha, nil
	}
	name, ok := ref.Name()
	if !ok {
		return "", fmt.Errorf("resolving %s/%s: empty ref", owner, repo)
	}
	var target string
	for _, qualified := range []string{name.TagRef(), name.BranchRef()} {
		sha, found, err := c.commitSHA(ctx, owner, repo, qualified)
		if err != nil {
			return "", err
		}
		switch {
		case !found:
		case target != "" && !strings.EqualFold(target, sha):
			return "", &AmbiguousRefError{Owner: owner, Repo: repo, Ref: name.String()}
		default:
			target = sha
		}
	}
	if target == "" {
		return "", &RefNotFoundError{Owner: owner, Repo: repo, Ref: name.String(),
			Err: fmt.Errorf("ref %q is not a tag or branch in %s/%s", name, owner, repo)}
	}
	return target, nil
}

// ResolveBranch returns the commit at refs/heads/<name>.
func (c *Client) ResolveBranch(ctx context.Context, owner, repo string, name parserlock.RefName) (string, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	sha, found, err := c.commitSHA(ctx, owner, repo, name.BranchRef())
	if err != nil {
		return "", err
	}
	if !found {
		return "", &RefNotFoundError{Owner: owner, Repo: repo, Ref: name.String(),
			Err: fmt.Errorf("branch %q does not exist in %s/%s", name, owner, repo)}
	}
	return sha, nil
}

// commitSHA reads repos/commits for an exact commit or a fully qualified ref
// name. found is false on 404 or 422.
func (c *Client) commitSHA(ctx context.Context, owner, repo, rev string) (sha string, found bool, err error) {
	path := fmt.Sprintf("repos/%s/%s/commits/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(rev))

	var result struct {
		SHA string `json:"sha"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &result); err != nil {
		if IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
			err = c.anonGet(ctx, path, &result)
			if code, _ := StatusCode(err); code == http.StatusNotFound || code == http.StatusUnprocessableEntity {
				return "", false, nil
			}
			if err != nil {
				return "", false, fmt.Errorf("anonymous fallback resolving %s/%s@%s: %w", owner, repo, rev, err)
			}
			return result.SHA, true, nil
		}
		if code, _ := StatusCode(err); code == http.StatusNotFound || code == http.StatusUnprocessableEntity {
			return "", false, nil
		}
		return "", false, fmt.Errorf("resolving %s/%s@%s: %w", owner, repo, rev, c.ssoErr(owner, err))
	}
	return result.SHA, true, nil
}

// CommitDate returns the committer timestamp of an exact commit, not its push time.
func (c *Client) CommitDate(ctx context.Context, owner, repo, sha string) (string, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	path := fmt.Sprintf("repos/%s/%s/commits/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(sha))
	var result struct {
		SHA    string `json:"sha"`
		Commit struct {
			Committer struct {
				Date string `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &result); err != nil {
		if IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
			if err := c.anonGet(ctx, path, &result); err != nil {
				return "", fmt.Errorf("fetching commit date for %s/%s@%s: %w", owner, repo, sha, err)
			}
		} else {
			return "", fmt.Errorf("fetching commit date for %s/%s@%s: %w", owner, repo, sha, c.ssoErr(owner, err))
		}
	}
	if !strings.EqualFold(result.SHA, sha) {
		return "", fmt.Errorf("commit date response for %s/%s@%s identified %q", owner, repo, sha, result.SHA)
	}
	return result.Commit.Committer.Date, nil
}

// ReleaseByTag looks up a release outside the recent-release listing.
// A 404 means no visible release; other failures must not become missing dates.
func (c *Client) ReleaseByTag(ctx context.Context, owner, repo, tag string) (*RepoRelease, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("repos/%s/%s/releases/tags/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(tag))
	var result struct {
		TagName     string `json:"tag_name"`
		PublishedAt string `json:"published_at"`
		Immutable   bool   `json:"immutable"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &result); err != nil {
		if IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
			err = c.anonGet(ctx, path, &result)
		}
		if err != nil {
			var httpErr *api.HTTPError
			if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
				return nil, nil
			}
			return nil, fmt.Errorf("fetching release for %s/%s@%s: %w", owner, repo, tag, c.ssoErr(owner, err))
		}
	}
	if result.TagName != tag {
		return nil, fmt.Errorf("release response for %s/%s@%s identified %q", owner, repo, tag, result.TagName)
	}
	return &RepoRelease{TagName: result.TagName, PublishedAt: result.PublishedAt, Immutable: result.Immutable}, nil
}
