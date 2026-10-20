package ghapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// This file is the one place a tag or branch name is looked up by exact
// name. Every caller (parity, reverse lookup, shadow checks) goes through
// it, so each gets the same rules:
//
//   - a name the Actions backend doesn't accept is never looked up;
//   - a name is looked up as refs/tags/<name>, then refs/heads/<name>, and
//     only a ref that exists by exactly that name counts (never a revision
//     expression, never a prefix match);
//   - a failed lookup is an error, never "not found";
//   - listings read every page;
//   - requests go through restGet, which handles the Dependabot proxy and
//     the anonymous fallback for public repositories behind SSO.
//
// GraphQL parity batches the same lookup as ref(qualifiedName:) in one
// query (see buildPinCheckQuery); refNames gives both paths the same names.

// refNames lists the full names name is looked up as, in order: a tag, then
// a branch. It is nil for a name the Actions backend doesn't accept; a refs/
// name is prefixed too, so it never resolves.
func refNames(name string) []string {
	if !AcceptedRefName(name) {
		return nil
	}
	return []string{"refs/tags/" + name, "refs/heads/" + name}
}

// refs lists the full names p's Head is looked up as.
func (p PinCheck) refs() []string { return refNames(p.Head) }

// ResolveRefName returns the commit name points at, looked up the way the
// Actions backend resolver looks up a lock entry ref; "" when name is not a
// tag or branch, or not a name the backend accepts.
func (c *Client) ResolveRefName(ctx context.Context, owner, repo, name string) (string, error) {
	for _, qualified := range refNames(name) {
		sha, found, err := c.ExactRef(ctx, owner, repo, qualified)
		if err != nil || found {
			return sha, err
		}
	}
	return "", nil
}

// TagExists reports whether owner/repo has a tag named exactly name.
func (c *Client) TagExists(ctx context.Context, owner, repo, name string) (bool, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return false, err
	}
	ref, found, err := c.gitRef(ctx, owner, repo, "refs/tags/"+name)
	return found && ref.Object.SHA != "", err
}

// ExactRef returns the commit the exact ref qualified (refs/tags/x or
// refs/heads/x) peels to.
func (c *Client) ExactRef(ctx context.Context, owner, repo, qualified string) (string, bool, error) {
	ref, found, err := c.gitRef(ctx, owner, repo, qualified)
	if err != nil || !found || ref.Object.SHA == "" {
		return "", false, err
	}
	if ref.Object.Type == "commit" {
		return ref.Object.SHA, true, nil
	}
	// An annotated tag: the ref exists, and a ref is resolved before any
	// other reading of the name, so commits/{ref} peels this one.
	return c.restCommit(ctx, owner, repo, qualified)
}

type gitRefBody struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}

// gitRef reads git/ref/<qualified without refs/>. found is false unless a
// ref exists by exactly that name: git/ref answers a name that only prefixes
// other refs with an array.
func (c *Client) gitRef(ctx context.Context, owner, repo, qualified string) (gitRefBody, bool, error) {
	var raw json.RawMessage
	path := fmt.Sprintf("repos/%s/%s/git/ref/%s", url.PathEscape(owner), url.PathEscape(repo), escapeBranchPath(strings.TrimPrefix(qualified, "refs/")))
	found, err := c.restGet(ctx, owner, path, &raw)
	if err != nil || !found {
		return gitRefBody{}, false, err
	}
	var ref gitRefBody
	if json.Unmarshal(raw, &ref) != nil || ref.Ref != qualified {
		return gitRefBody{}, false, nil
	}
	return ref, true, nil
}

// TagsAtCommit returns the names of every tag in owner/repo whose commit is
// sha, reading every page of the tag listing (one request per 100 tags).
// Use it only where a later page matters: ListTags reads the first page.
func (c *Client) TagsAtCommit(ctx context.Context, owner, repo, sha string) ([]string, error) {
	c, err := c.ForRepo(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var names []string
	for page := 1; ; page++ {
		path := fmt.Sprintf("repos/%s/%s/tags?per_page=100&page=%d", url.PathEscape(owner), url.PathEscape(repo), page)
		var resp []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		found, err := c.restGet(ctx, owner, path, &resp)
		if err != nil {
			return nil, fmt.Errorf("listing tags for %s/%s: %w", owner, repo, err)
		}
		if !found {
			return names, nil
		}
		for _, t := range resp {
			if strings.EqualFold(t.Commit.SHA, sha) {
				names = append(names, t.Name)
			}
		}
		if len(resp) < 100 {
			return names, nil
		}
	}
}

func (c *Client) restCommit(ctx context.Context, owner, repo, ref string) (string, bool, error) {
	var commit struct {
		SHA string `json:"sha"`
	}
	path := fmt.Sprintf("repos/%s/%s/commits/%s", url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(ref))
	found, err := c.restGet(ctx, owner, path, &commit)
	if err != nil || !found {
		return "", false, err
	}
	return commit.SHA, true, nil
}

// restGet reports found=false for a 404 or 422. It reads anonymously under
// the Dependabot proxy, and falls back to an anonymous read for a public
// repository behind an organization's SSO.
func (c *Client) restGet(ctx context.Context, owner, path string, dest any) (bool, error) {
	var err error
	if c.restOnly {
		err = c.anonGet(ctx, path, dest)
	} else {
		err = c.rest.DoWithContext(ctx, http.MethodGet, path, nil, dest)
		if err != nil && IsSAMLEnforcement(err) && c.SSOFallbackEligible(ctx, owner) {
			err = c.anonGet(ctx, path, dest)
		}
	}
	if code, _ := StatusCode(err); code == http.StatusNotFound || code == http.StatusUnprocessableEntity {
		return false, nil
	}
	if err != nil {
		return false, c.ssoErr(owner, err)
	}
	return true, nil
}
