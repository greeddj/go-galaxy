// Package galaxyv1 maps a Galaxy role name to a GitHub repository and tag via
// the v1 role API; the answer is a pointer, never content: download_url is not
// read, every record field is validated, and paging stays on the server origin.
package galaxyv1

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/safeout"
)

// FetchJSON is the cache-policy-aware JSON fetch the caller supplies: the
// collections pipeline's own, bound to its HTTP client, store and deadline.
type FetchJSON func(ctx context.Context, url string, out any, policy cacheManager.Policy) error

// pageSize is what ansible-galaxy asks the v1 API for, per page.
const pageSize = 50

// defaultBranch is what ansible-galaxy falls back to when a role lists no
// versions and its record names no branch.
const defaultBranch = "master"

// gitHubHost is the one host a v1 role record points at: the v1 API imports
// roles from GitHub and names them by user and repository.
const gitHubHost = "github.com"

// Role is the part of a v1 role record this tool reads: the GitHub user and
// repository the role is imported from, its default branch, and the id the
// version list is keyed by.
type Role struct {
	GitHubUser   string
	GitHubRepo   string
	GitHubBranch string
	ID           int64
}

// Version is one entry of a role's v1 version list: the tag name, which is
// what ansible-galaxy downloads and what this tool fetches, and the commit
// the server recorded for it when it recorded one.
type Version struct {
	Name      string
	CommitSHA string
}

// Resolution is a Galaxy role's answer: the repository, a qualified ref (so a
// branch sharing a tag's name is never fetched instead), the version it names,
// the commit the server recorded for the tag if any, and the listed tag names.
type Resolution struct {
	RepoURL   gitsource.URL
	Ref       gitsource.Ref
	Version   string
	GalaxySHA string
	Versions  []string
}

// roleListPage is the v1 list response shape this tool decodes.
type roleListPage struct {
	Results []roleRecord `json:"results"`
}

type roleRecord struct {
	GitHubUser   string `json:"github_user"`
	GitHubRepo   string `json:"github_repo"`
	GitHubBranch string `json:"github_branch"`
	ID           int64  `json:"id"`
}

// versionsPage is the v1 versions response shape this tool decodes. next is
// the full URL older servers send and next_link the path Galaxy NG sends;
// either is followed when present.
type versionsPage struct {
	Next     *string         `json:"next"`
	NextLink *string         `json:"next_link"`
	Results  []versionRecord `json:"results"`
}

type versionRecord struct {
	CommitSHA *string `json:"commit_sha"`
	Name      string  `json:"name"`
}

// cleanForMessage renders a server-supplied string for a message: control
// runes stripped and the length bounded.
func cleanForMessage(s string) string {
	return helpers.TruncateForMessage(string(safeout.Clean(s)))
}

// apiRoots derives the v1 roots a server base may serve roles under, in probe
// order: galaxy.ansible.com's <base>/api/v1, then Galaxy NG's <base>/v1, which
// is the only root for a base already ending in /api.
func apiRoots(base string) []string {
	trimmed := strings.TrimRight(strings.TrimSpace(base), "/")
	if trimmed == "" {
		return nil
	}
	if strings.HasSuffix(trimmed, "/api") {
		return []string{trimmed + "/v1"}
	}
	return []string{trimmed + "/api/v1", trimmed + "/v1"}
}

// LookupRole asks the server for owner.name; found is false when its v1 API
// lists no such role. A 404 on every root is helpers.ErrGalaxyRoleAPIUnavailable,
// for the caller to route around; 401/403 and retryable statuses abort the walk.
func LookupRole(ctx context.Context, fetch FetchJSON, base, owner, name string, policy cacheManager.Policy) (Role, bool, error) {
	query := "roles/?owner__username=" + url.QueryEscape(owner) + "&name=" + url.QueryEscape(name) +
		"&page_size=" + strconv.Itoa(pageSize)
	var lastErr error
	for _, root := range apiRoots(base) {
		var page roleListPage
		err := fetch(ctx, root+"/"+query, &page, policy)
		if err == nil {
			if len(page.Results) == 0 {
				return Role{}, false, nil
			}
			role, err := validateRole(page.Results[0])
			return role, err == nil, err
		}
		if statusErr, ok := errors.AsType[*cacheManager.HTTPStatusError](err); ok {
			switch {
			case statusErr.Code == http.StatusNotFound:
				lastErr = err
				continue
			case statusErr.Code == http.StatusUnauthorized, statusErr.Code == http.StatusForbidden:
				return Role{}, false, fmt.Errorf("%w: %w", helpers.ErrGalaxyAuthFailed, err)
			case helpers.IsRetryableHTTPStatus(statusErr.Code):
				return Role{}, false, fmt.Errorf("%w: %w", helpers.ErrGalaxyServerUnavailable, err)
			}
		}
		return Role{}, false, err
	}
	return Role{}, false, fmt.Errorf("%w: %s answers 404 for the v1 role API: %w",
		helpers.ErrGalaxyRoleAPIUnavailable, helpers.URLForMessage(base), lastErr)
}

// validateRole judges a role record the way every server-supplied identity
// is judged on the way in: the GitHub user and repository against the GitHub
// name alphabet, the branch against the ref grammar.
func validateRole(rec roleRecord) (Role, error) {
	if !isGitHubName(rec.GitHubUser) || !isGitHubName(rec.GitHubRepo) {
		return Role{}, fmt.Errorf("%w: github_user %q / github_repo %q", helpers.ErrGalaxyRoleInvalid,
			cleanForMessage(rec.GitHubUser), cleanForMessage(rec.GitHubRepo))
	}
	branch := strings.TrimSpace(rec.GitHubBranch)
	if branch != "" {
		ref, err := gitsource.ParseRef("refs/heads/" + branch)
		if err != nil || ref.Kind != gitsource.RefQualified || !helpers.IsRoleVersion(branch) {
			return Role{}, fmt.Errorf("%w: github_branch %q", helpers.ErrGalaxyRoleInvalid, cleanForMessage(branch))
		}
	}
	return Role{GitHubUser: rec.GitHubUser, GitHubRepo: rec.GitHubRepo, GitHubBranch: branch, ID: rec.ID}, nil
}

// gitHubNameMaxLen bounds a GitHub user or repository name; GitHub caps a
// login at 39 characters and a repository at 100.
const gitHubNameMaxLen = 100

// isGitHubName reports whether s is a GitHub login or repository name; the
// alphabet keeps a record from composing a URL path gitsource would refuse,
// or one that reads as a different repository.
func isGitHubName(s string) bool {
	if s == "" || len(s) > gitHubNameMaxLen || s == ".." || s[0] == '-' || s[0] == '.' {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return !isGitHubRune(r) })
}

func isGitHubRune(r rune) bool {
	return isASCIILetter(r) || isASCIIDigit(r) || r == '-' || r == '_' || r == '.'
}

func isASCIILetter(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

func isLowerLetter(r rune) bool { return r >= 'a' && r <= 'z' }

// ListVersions walks the role's version pages, following the server's next
// link within its origin for at most helpers.RoleVersionsMaxPages pages; a
// name the ref grammar refuses is dropped, as is a malformed commit sha.
func ListVersions(ctx context.Context, fetch FetchJSON, base string, id int64, policy cacheManager.Policy) ([]Version, []string, error) {
	var (
		out      []Version
		warnings []string
		lastErr  error
	)
	for _, root := range apiRoots(base) {
		first := fmt.Sprintf("%s/roles/%d/versions/?page_size=%d", root, id, pageSize)
		versions, pageWarnings, err := walkVersions(ctx, fetch, first, policy)
		if err == nil {
			return versions, pageWarnings, nil
		}
		if statusErr, ok := errors.AsType[*cacheManager.HTTPStatusError](err); ok && statusErr.Code == http.StatusNotFound {
			lastErr = err
			continue
		}
		return nil, nil, err
	}
	return out, warnings, fmt.Errorf("%w: %w", helpers.ErrGalaxyRoleAPIUnavailable, lastErr)
}

// walkVersions follows one root's version pages.
func walkVersions(ctx context.Context, fetch FetchJSON, first string, policy cacheManager.Policy) ([]Version, []string, error) {
	origin, err := url.Parse(first)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", helpers.ErrGalaxyRoleInvalid, err)
	}
	var out []Version
	var warnings []string
	next := first
	for page := 0; next != ""; page++ {
		if page >= helpers.RoleVersionsMaxPages {
			return nil, nil, fmt.Errorf("%w: more than %d pages of versions", helpers.ErrVersionsPagingExceeded, helpers.RoleVersionsMaxPages)
		}
		var body versionsPage
		if err := fetch(ctx, next, &body, policy); err != nil {
			return nil, nil, err
		}
		for _, rec := range body.Results {
			v, ok, warning := validateVersion(rec)
			if warning != "" {
				warnings = append(warnings, warning)
			}
			if ok {
				out = append(out, v)
			}
		}
		next, err = nextPage(origin, body)
		if err != nil {
			return nil, nil, err
		}
	}
	return out, warnings, nil
}

// validateVersion drops, with a warning, a name that is not a tag name judged
// as refs/tags/<name> (so an all-digit date tag is never an abbreviated
// commit), and silently drops a commit sha that is not forty hex digits.
func validateVersion(rec versionRecord) (Version, bool, string) {
	name := strings.TrimSpace(rec.Name)
	if name == "" || !helpers.IsRoleVersion(name) || !isTagName(name) {
		return Version{}, false, "skipping version " + cleanForMessage(name) + ": not a tag name"
	}
	v := Version{Name: name}
	if rec.CommitSHA != nil && gitsource.IsCommitHash(strings.ToLower(strings.TrimSpace(*rec.CommitSHA))) {
		v.CommitSHA = strings.ToLower(strings.TrimSpace(*rec.CommitSHA))
	}
	return v, true, ""
}

// nextPage resolves the page's next link against the first page's URL and
// refuses one that leaves its origin: a link is server content, and the
// Galaxy client's token is attached by origin.
func nextPage(origin *url.URL, body versionsPage) (string, error) {
	var link string
	switch {
	case body.NextLink != nil && strings.TrimSpace(*body.NextLink) != "":
		link = strings.TrimSpace(*body.NextLink)
	case body.Next != nil && strings.TrimSpace(*body.Next) != "":
		link = strings.TrimSpace(*body.Next)
	default:
		return "", nil
	}
	ref, err := url.Parse(link)
	if err != nil {
		return "", fmt.Errorf("%w: next page link: %w", helpers.ErrGalaxyRoleInvalid, err)
	}
	resolved := origin.ResolveReference(ref)
	if resolved.Scheme != origin.Scheme || resolved.Host != origin.Host || resolved.User != nil {
		return "", fmt.Errorf("%w: next page link %s leaves %s", helpers.ErrGalaxyRoleInvalid,
			helpers.URLForMessage(resolved.String()), origin.Host)
	}
	return resolved.String(), nil
}

// Resolve maps owner.name at the requested version ("" for the highest) to
// the repository and ref to fetch. found is false when the server has the
// v1 API and does not know the role.
func Resolve(
	ctx context.Context, fetch FetchJSON, base, owner, name, requested string, policy cacheManager.Policy,
) (Resolution, bool, []string, error) {
	role, found, err := LookupRole(ctx, fetch, base, owner, name, policy)
	if err != nil || !found {
		return Resolution{}, found, nil, err
	}
	versions, warnings, err := ListVersions(ctx, fetch, base, role.ID, policy)
	if err != nil {
		return Resolution{}, true, warnings, err
	}
	version, sha, err := Select(versions, requested, role.GitHubBranch)
	if err != nil {
		return Resolution{}, true, warnings, fmt.Errorf("%s.%s: %w", owner, name, err)
	}
	repo, err := gitsource.ParseURL("https://" + gitHubHost + "/" + role.GitHubUser + "/" + role.GitHubRepo)
	if err != nil {
		return Resolution{}, true, warnings, fmt.Errorf("%w: %w", helpers.ErrGalaxyRoleInvalid, err)
	}
	ref, err := qualifiedRef(versions, version)
	if err != nil {
		return Resolution{}, true, warnings, err
	}
	return Resolution{RepoURL: repo, Ref: ref, Version: version, GalaxySHA: sha, Versions: versionNames(versions)}, true, warnings, nil
}

// qualifiedRef spells the chosen version as the one ref it means: a listed
// tag is refs/tags/<tag>, anything else (the default branch, master, a
// version asked for on a role with no tags) is refs/heads/<name>.
func qualifiedRef(versions []Version, version string) (gitsource.Ref, error) {
	prefix := "refs/heads/"
	for _, v := range versions {
		if v.Name == version {
			prefix = "refs/tags/"
			break
		}
	}
	ref, err := gitsource.ParseRef(prefix + version)
	if err != nil {
		return gitsource.Ref{}, fmt.Errorf("%w: %w", helpers.ErrGalaxyRoleInvalid, err)
	}
	return ref, nil
}

// isTagName reports whether name is a tag name git would accept, judged as
// the qualified refs/tags/<name> so the unqualified grammar's abbreviated-
// commit refusal does not apply to an all-digit tag.
func isTagName(name string) bool {
	ref, err := gitsource.ParseRef("refs/tags/" + name)
	return err == nil && ref.Kind == gitsource.RefQualified
}

// ValidateRepository judges a recorded Galaxy pin's repository as a live v1
// answer is judged, https://github.com/<user>/<repo> in the GitHub alphabet,
// since cache state may not point a role anywhere a server could not have.
func ValidateRepository(u gitsource.URL) error {
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Scheme != "https" || u.Host != gitHubHost || u.Port != "" || len(segments) != 2 ||
		!isGitHubName(segments[0]) || !isGitHubName(strings.TrimSuffix(segments[1], ".git")) {
		return fmt.Errorf("%w: repository %s is not a GitHub repository", helpers.ErrGalaxyRoleInvalid, helpers.URLForMessage(u.String()))
	}
	return nil
}

// Select chooses the version as ansible's GalaxyRole.install does: the highest
// tag by LooseVersion, else the default branch or "master"; a requested one must
// be a listed tag, the default branch or "master", unless no tags are listed.
func Select(versions []Version, requested, githubBranch string) (string, string, error) {
	branch := githubBranch
	if branch == "" {
		branch = defaultBranch
	}
	if requested == "" {
		if len(versions) == 0 {
			return branch, "", nil
		}
		best, err := highest(versions)
		if err != nil {
			return "", "", err
		}
		return best.Name, best.CommitSHA, nil
	}
	return selectRequested(versions, requested, branch)
}

// selectRequested finds the requested version among the listed tags, or
// lets the default branch (and ansible's literal master) through unlisted,
// as does any version when the server lists no tags at all.
func selectRequested(versions []Version, requested, branch string) (string, string, error) {
	for _, v := range versions {
		if v.Name == requested {
			return v.Name, v.CommitSHA, nil
		}
	}
	if requested == branch || requested == defaultBranch || len(versions) == 0 {
		return requested, "", nil
	}
	return "", "", fmt.Errorf("%w: %s is not among the listed versions %v", helpers.ErrRoleVersionNotFound, requested, versionNames(versions))
}

// versionNames lists the tag names, in the order listed.
func versionNames(versions []Version) []string {
	names := make([]string, 0, len(versions))
	for _, v := range versions {
		names = append(names, v.Name)
	}
	return names
}

// highest returns the LooseVersion maximum of versions, ties broken by the
// lexicographically greater name so the choice is deterministic where
// ansible's depends on server order.
func highest(versions []Version) (Version, error) {
	best := versions[0]
	for _, v := range versions[1:] {
		less, ok := LooseLess(best.Name, v.Name)
		if !ok {
			return Version{}, fmt.Errorf("%w: %q and %q have incompatible formats; specify an explicit role version to install",
				helpers.ErrRoleVersionsIncomparable, best.Name, v.Name)
		}
		if less || (!less && looseEqual(best.Name, v.Name) && v.Name > best.Name) {
			best = v
		}
	}
	return best, nil
}

// looseEqual reports whether two names have equal LooseVersion components.
func looseEqual(a, b string) bool {
	ca, cb := looseComponents(a), looseComponents(b)
	if len(ca) != len(cb) {
		return false
	}
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}

// looseComponent is one LooseVersion component: a number or a text run.
type looseComponent struct {
	text   string
	number int
	isNum  bool
}

// looseComponents splits a version the way distutils' LooseVersion does:
// runs of digits become numbers, runs of lower-case letters and any other
// run between them become text, and dots are dropped.
func looseComponents(s string) []looseComponent {
	var out []looseComponent
	for i := 0; i < len(s); {
		r := rune(s[i])
		if r == '.' {
			i++
			continue
		}
		class := looseClass(r)
		j := i + 1
		for j < len(s) && looseClass(rune(s[j])) == class {
			j++
		}
		if class == classDigit {
			out = append(out, looseComponent{number: parseDigits(s[i:j]), isNum: true})
		} else {
			out = append(out, looseComponent{text: s[i:j]})
		}
		i = j
	}
	return out
}

// looseRuneClass is how LooseVersion's tokenizer classes a byte: a digit, a
// lower-case letter, a dot, or anything else.
type looseRuneClass uint8

const (
	classOther looseRuneClass = iota
	classDigit
	classLower
	classDot
)

func looseClass(r rune) looseRuneClass {
	switch {
	case isASCIIDigit(r):
		return classDigit
	case isLowerLetter(r):
		return classLower
	case r == '.':
		return classDot
	default:
		return classOther
	}
}

// parseDigits reads a run of ASCII digits, saturating rather than wrapping
// on a run longer than an int holds - such a "version" orders above every
// real one, which is what a number that large means.
func parseDigits(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return math.MaxInt
	}
	return n
}

// LooseLess reports whether a orders before b under distutils' LooseVersion;
// ok is false when a number meets a text component, where Python raises and
// ansible reports the versions as incomparable.
func LooseLess(a, b string) (bool, bool) {
	ca, cb := looseComponents(a), looseComponents(b)
	for i := 0; i < len(ca) && i < len(cb); i++ {
		x, y := ca[i], cb[i]
		if x.isNum != y.isNum {
			return false, false
		}
		if x == y {
			continue
		}
		if x.isNum {
			return x.number < y.number, true
		}
		return x.text < y.text, true
	}
	return len(ca) < len(cb), true
}
