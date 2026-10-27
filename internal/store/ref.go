package store

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Ref identifies an item: a number in a repository, or an advisory's GHSA id.
type Ref struct {
	// Repo is "owner/name". For an advisory it is the main repository.
	Repo   string
	Number int
	GHSA   string
}

// ghsaPattern matches a GHSA id in any case. GitHub writes the prefix upper
// case and the rest lower case, which is how ParseRef normalises it.
var ghsaPattern = regexp.MustCompile(`(?i)^ghsa(-[0-9a-z]{4}){3}$`)

// IsAdvisory reports whether the ref names an advisory.
func (r Ref) IsAdvisory() bool { return r.GHSA != "" }

// Display renders the ref as a user would type it, relative to the main
// repository: "#123", "owner/fork#2" or the GHSA id.
func (r Ref) Display(mainRepo string) string {
	switch {
	case r.GHSA != "":
		return r.GHSA
	case r.Repo == mainRepo:
		return fmt.Sprintf("#%d", r.Number)
	default:
		return fmt.Sprintf("%s#%d", r.Repo, r.Number)
	}
}

// ParseRef reads a reference to an item of the main repository ("owner/name"):
//
//   - "#123" or "123", an issue or pull request in the main repository
//   - a GitHub URL for an issue, pull request or advisory
//   - a GHSA id, "GHSA-xxxx-xxxx-xxxx"
//   - "fork#2" or "owner/fork#2", a pull request in a private fork
//
// Only the main repository and its advisories' private forks are accepted:
// triage-bot keeps notes on one project, not on GitHub at large.
func ParseRef(s, mainRepo string) (Ref, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Ref{}, fmt.Errorf("empty reference")
	case ghsaPattern.MatchString(s):
		return Ref{Repo: mainRepo, GHSA: normaliseGHSA(s)}, nil
	case strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://"):
		return parseURL(s, mainRepo)
	}

	repo, num, found := strings.Cut(s, "#")
	if !found {
		repo, num = "", s
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return Ref{}, fmt.Errorf("%q is not a reference: use #123, a GitHub URL, a GHSA id or fork#N", s)
	}
	full, err := resolveRepo(repo, mainRepo)
	if err != nil {
		return Ref{}, err
	}
	return Ref{Repo: full, Number: n}, nil
}

// parseURL reads https://github.com/OWNER/REPO/{issues,pull}/N and
// https://github.com/OWNER/REPO/security/advisories/GHSA-....
func parseURL(s, mainRepo string) (Ref, error) {
	u, err := url.Parse(s)
	if err != nil || u.Host != "github.com" {
		return Ref{}, fmt.Errorf("%q is not a github.com URL", s)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 {
		return Ref{}, fmt.Errorf("%q does not point at an issue, pull request or advisory", s)
	}
	repo, err := resolveRepo(parts[0]+"/"+parts[1], mainRepo)
	if err != nil {
		return Ref{}, err
	}

	switch {
	case (parts[2] == "issues" || parts[2] == "pull"):
		n, err := strconv.Atoi(parts[3])
		if err != nil || n <= 0 {
			return Ref{}, fmt.Errorf("%q has no item number", s)
		}
		return Ref{Repo: repo, Number: n}, nil
	case parts[2] == "security" && len(parts) >= 5 && parts[3] == "advisories" && ghsaPattern.MatchString(parts[4]):
		if repo != mainRepo {
			return Ref{}, fmt.Errorf("%q: advisories belong to %s", s, mainRepo)
		}
		return Ref{Repo: mainRepo, GHSA: normaliseGHSA(parts[4])}, nil
	default:
		return Ref{}, fmt.Errorf("%q does not point at an issue, pull request or advisory", s)
	}
}

// resolveRepo expands a repository named in a ref to "owner/name" and checks
// it is one we keep notes on. An empty name is the main repository; a bare
// name is taken to share the main repository's owner.
func resolveRepo(repo, mainRepo string) (string, error) {
	owner, _, _ := strings.Cut(mainRepo, "/")
	switch {
	case repo == "":
		return mainRepo, nil
	case !strings.Contains(repo, "/"):
		repo = owner + "/" + repo
	}
	if strings.EqualFold(repo, mainRepo) {
		return mainRepo, nil
	}
	if IsPrivateFork(repo, mainRepo) {
		return strings.ToLower(repo), nil
	}
	return "", fmt.Errorf("%s is not %s or one of its advisories' private forks", repo, mainRepo)
}

// IsPrivateFork reports whether repo is the shape of one of mainRepo's
// advisory forks: GitHub names them "<repo>-ghsa-xxxx-xxxx-xxxx" under the
// same owner.
func IsPrivateFork(repo, mainRepo string) bool {
	prefix := strings.ToLower(mainRepo) + "-"
	lower := strings.ToLower(repo)
	return strings.HasPrefix(lower, prefix) && ghsaPattern.MatchString(strings.TrimPrefix(lower, prefix))
}

func normaliseGHSA(s string) string {
	return "GHSA" + strings.ToLower(s[len("GHSA"):])
}
