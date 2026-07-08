// Skycaptain: Forgejo Actions Updater
//
// TODO: Use Forgejo's Tags API to fetch tags instead of git ls-remote?
//
// SPDX-License-Identifier: BSD-3-Clause
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"charm.land/log/v2"
	"github.com/Masterminds/semver/v3"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/goccy/go-yaml"
	"github.com/jdx/go-netrc"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Config holds the runtime configuration for the updater.
type Config struct {
	DryRun    bool
	UseSSH    bool
	NetrcPath string
	Bump      bool   // Update to the latest semver tag (default: true)
	Lock      bool   // Lock to a git commit SHA instead of a tag reference (default: true)
	Template  string // Custom Go template for rendering action refs (empty = use default)
}

// ParsedActionReference represents a uses: action reference found in a Forgejo workflow file.
type ParsedActionReference struct {
	RepoURL string // Full https URL without @ref (e.g., https://github.com/actions/checkout)
	Ref     string // Current ref (SHA or tag)
	Comment string // Comment text after # if present (e.g., "v6.0.2")
}

// ActionReference holds the resolved details for an action reference, ready for rendering.
type ActionReference struct {
	GitURL     string
	Tag        string
	FullCommit string
	// Semver components (zero/empty when Tag is not valid semver)
	Major      uint64
	Minor      uint64
	Patch      uint64
	RawVersion string // "{Major}.{Minor}.{Patch}" e.g. "4.0.0"
}

// remoteData holds the cached result of a remote repository tag listing.
type remoteData struct {
	tagCommits map[string]string
	tagNames   []string
}

type Updater struct {
	config *Config
	cache  map[string]*remoteData
}

var errNoGitRepositoryFound = errors.New("no git repository found")
var errNoHTTPAuth = errors.New("no HTTP authentication available")
var errNoTagsFound = errors.New("no tags found for repository")
var errNoValidSemverTags = errors.New("no valid semver tags found")
var errTagNotFoundInRepo = errors.New("tag not found in repository")

// actionRefTmpl is the default template for rendering an updated action reference.
const actionRefTmpl = `uses: {{ .GitURL }}@{{ if .Lock }}{{ .FullCommit }}  # v{{ .RawVersion }}{{ else }}{{ .Tag }}{{ end }}`

// actionRefRe parses a uses: action reference with named capture groups:
//   - url:     repository URL or short owner/repo form
//   - ref:     commit SHA or tag after @
//   - comment: optional inline version hint after #
var actionRefRe = regexp.MustCompile(`uses:[ \t]+(?P<url>[^\s@]+)@(?P<ref>[^\s#]+)(?:[ \t]*#(?P<comment>.*))?`)

func main() {
	var (
		searchPath = flag.String("path", "", "Explicit path to the project root containing .forgejo/workflows/ (default: auto-detect from git root)")
		dryRun     = flag.Bool("dry-run", false, "Show what would be updated without making changes")
		verbose    = flag.Bool("verbose", false, "Enable verbose logging")
		useSSH     = flag.Bool("ssh", false, "Use SSH authentication via SSH agent for Git operations")
		netrcPath  = flag.String("netrc", "", "Path to .netrc file for HTTP auth (default: ~/.netrc)")
		bump       = flag.Bool("bump", true, "Update to the latest semver tag; keep the existing ref if set to false")
		lock       = flag.Bool("lock", true, "Lock to a git commit SHA; keep a tag reference if set to false")
		tmpl       = flag.String("template", actionRefTmpl, "Go template for rendering updated action refs (available: .GitURL .Tag .FullCommit .Lock .Major .Minor .Patch .RawVersion)")
	)

	flag.Parse()

	if *verbose {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}

	//nolint:nestif
	if *netrcPath == "" {
		if envNetrc := os.Getenv("NETRC"); envNetrc != "" {
			*netrcPath = envNetrc
		} else {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				log.Warn("Failed to get user home directory", "error", err)
			} else {
				*netrcPath = filepath.Join(homeDir, ".netrc")
			}
		}
	}

	cfg := &Config{
		DryRun:    *dryRun,
		UseSSH:    *useSSH,
		NetrcPath: *netrcPath,
		Bump:      *bump,
		Lock:      *lock,
		Template:  *tmpl,
	}

	updater := NewUpdater(cfg)

	err := updater.UpdateWorkflows(*searchPath)
	if err != nil {
		log.Fatal("Failed to update workflows", "error", err)
	}
}

// NewUpdater creates a new Updater with the given configuration.
func NewUpdater(cfg *Config) *Updater {
	return &Updater{config: cfg, cache: make(map[string]*remoteData)}
}

// UpdateWorkflows discovers and updates all Forgejo workflow files under searchPath.
func (up *Updater) UpdateWorkflows(searchPath string) error {
	if !up.config.Bump && !up.config.Lock {
		log.Info("Nothing to do: --bump=false --lock=false")

		return nil
	}

	files, err := up.findForgejoWorkflows(searchPath)
	if err != nil {
		return fmt.Errorf("failed to find Forgejo workflow files: %w", err)
	}

	log.Info("Found Forgejo workflow files", "count", len(files))

	for _, file := range files {
		err := up.updateFile(file)
		if err != nil {
			return fmt.Errorf("failed to update file %s: %w", file, err)
		}
	}

	return nil
}

// findGitRoot returns the worktree root of the git repository that contains startPath.
//
// It uses go-git with DetectDotGit so that it walks up the directory tree automatically, the same
// way `git rev-parse --show-toplevel` would.
func findGitRoot(startPath string) (string, error) {
	abs, err := filepath.Abs(startPath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path %s: %w", startPath, err)
	}

	repo, err := git.PlainOpenWithOptions(abs, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return "", fmt.Errorf("%w from %s", errNoGitRepositoryFound, startPath)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree for %s: %w", startPath, err)
	}

	return wt.Filesystem.Root(), nil
}

// findForgejoWorkflows returns all YAML files under <root>/.forgejo/workflows/.
//
// If searchPath is empty the project root is auto-detected by walking up from the current working
// directory until a .git entry is found. If searchPath is explicitly provided it is used directly
// without any git-root traversal.
func (up *Updater) findForgejoWorkflows(searchPath string) ([]string, error) {
	var root string

	if searchPath != "" {
		// Explicit path: use as-is.
		root = searchPath
		log.Debug("Using explicit search path", "path", root)
	} else {
		// No explicit path: auto-detect from cwd.
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get working directory: %w", err)
		}

		gitRoot, err := findGitRoot(cwd)
		if err != nil {
			log.Warn("Could not locate git root, using current directory", "cwd", cwd, "error", err)
			root = cwd
		} else {
			log.Debug("Auto-detected git root", "path", gitRoot)
			root = gitRoot
		}
	}

	workflowsDir := filepath.Join(root, ".forgejo", "workflows")

	_, err := os.Stat(workflowsDir)
	if os.IsNotExist(err) {
		log.Debug("No .forgejo/workflows directory found", "path", workflowsDir)

		return nil, nil
	}

	var files []string

	err = filepath.Walk(workflowsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && (strings.HasSuffix(info.Name(), ".yaml") || strings.HasSuffix(info.Name(), ".yml")) {
			files = append(files, path)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk workflows directory: %w", err)
	}

	return files, nil
}

// updateFile reads a workflow file, resolves action references, and writes back any changes.
func (up *Updater) updateFile(filePath string) error {
	log.Debug("Processing file", "file", filePath)

	rawContent, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	tmpl, err := template.New("ref").Parse(up.config.Template)
	if err != nil {
		return fmt.Errorf("invalid action ref template: %w", err)
	}

	originalContent := string(rawContent)

	updatedContent := actionRefRe.ReplaceAllStringFunc(originalContent, func(match string) string {
		m := actionRefRe.FindStringSubmatch(match)
		ref := parseActionMatch(m)

		result, err := up.resolveAction(ref)
		if err != nil {
			log.Error("Failed to resolve action", "repo", ref.RepoURL, "error", err)

			return match
		}

		var buf strings.Builder

		err = tmpl.Execute(&buf, struct {
			ActionReference

			Lock bool
		}{
			ActionReference: *result,
			Lock:            up.config.Lock,
		})
		if err != nil {
			log.Error("Failed to render action ref", "repo", ref.RepoURL, "error", err)

			return match
		}

		newValue := buf.String()

		if newValue == match {
			log.Debug("Action already up-to-date", "repo", ref.RepoURL)

			return match
		}

		if up.config.DryRun {
			log.Info("Would update action", "file", filePath, "repo", ref.RepoURL, "from", match, "to", newValue)

			return match
		}

		log.Info("Updating action", "file", filePath, "repo", ref.RepoURL, "tag", result.Tag)

		return newValue
	})

	if updatedContent == originalContent {
		return nil
	}

	// Validate that the updated content is still valid YAML
	var yamlCheck any

	err = yaml.Unmarshal([]byte(updatedContent), &yamlCheck)
	if err != nil {
		return fmt.Errorf("updated content is not valid YAML: %w", err)
	}

	//nolint:mnd
	err = os.WriteFile(filePath, []byte(updatedContent), 0o644)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	log.Info("Updated file", "file", filePath)

	return nil
}

// parseActionMatch converts a FindStringSubmatch result from actionRefRe into an ActionReference.
func parseActionMatch(m []string) *ParsedActionReference {
	rawURL := m[actionRefRe.SubexpIndex("url")]
	ref := m[actionRefRe.SubexpIndex("ref")]
	comment := strings.TrimSpace(m[actionRefRe.SubexpIndex("comment")])

	var repoURL string
	if strings.HasPrefix(rawURL, "https://") || strings.HasPrefix(rawURL, "http://") {
		repoURL = rawURL
	} else {
		// Short form (e.g., actions/checkout) — default to github.com
		repoURL = "https://github.com/" + rawURL
	}

	return &ParsedActionReference{
		RepoURL: repoURL,
		Ref:     ref,
		Comment: comment,
	}
}

// fetchRemoteData fetches the tag list from a remote repository, caching the result per repo URL.
func (up *Updater) fetchRemoteData(repoURL string) (*remoteData, error) {
	if cached, ok := up.cache[repoURL]; ok {
		log.Debug("Using cached refs", "repo", repoURL)

		return cached, nil
	}

	gitURL := repoURL + ".git"

	log.Debug("Fetching refs from repository", "repo", gitURL)

	auth, err := up.getAuthMethod(gitURL)
	if err != nil {
		if !errors.Is(err, errNoHTTPAuth) {
			log.Warn("Authentication failed, trying without auth", "repo", gitURL, "error", err)
		}

		auth = nil
	}

	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin",
		URLs: []string{gitURL},
	})

	listOptions := &git.ListOptions{}
	if auth != nil {
		listOptions.Auth = auth
	}

	remoteRefs, err := remote.List(listOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to list remote refs for %s: %w", gitURL, err)
	}

	// Build tag -> commit SHA map.
	// Peeled refs (refs/tags/v1.0.0^{}) give the actual commit SHA for annotated tags
	// and take priority over the tag-object SHA.
	tagCommits := make(map[string]string)

	var tagNames []string

	// First pass: collect peeled (dereferenced annotated tag) SHAs
	for _, r := range remoteRefs {
		if !r.Name().IsTag() {
			continue
		}

		name := r.Name().Short()
		if before, ok := strings.CutSuffix(name, "^{}"); ok {
			tagCommits[before] = r.Hash().String()
		}
	}

	// Second pass: collect tag names and fill SHAs for lightweight tags
	for _, r := range remoteRefs {
		if !r.Name().IsTag() {
			continue
		}

		name := r.Name().Short()
		if strings.HasSuffix(name, "^{}") {
			continue
		}

		tagNames = append(tagNames, name)

		if _, alreadySet := tagCommits[name]; !alreadySet {
			tagCommits[name] = r.Hash().String()
		}
	}

	if len(tagNames) == 0 {
		return nil, fmt.Errorf("%w %s", errNoTagsFound, gitURL)
	}

	data := &remoteData{tagCommits: tagCommits, tagNames: tagNames}
	up.cache[repoURL] = data

	return data, nil
}

// resolveAction determines the target tag and (when locking) its commit SHA for an action.
func (up *Updater) resolveAction(ref *ParsedActionReference) (*ActionReference, error) {
	data, err := up.fetchRemoteData(ref.RepoURL)
	if err != nil {
		return nil, err
	}

	var tag string

	// Determine the target tag
	if up.config.Bump {
		tag, err = findLatestSemverTag(data.tagNames)
		if err != nil {
			return nil, err
		}
	} else {
		// Use the comment tag if present (human-readable version hint), else the current ref
		if ref.Comment != "" {
			tag = ref.Comment
		} else {
			tag = ref.Ref
		}
	}

	var commit string

	// Only resolve the commit when locking is enabled
	if up.config.Lock {
		var ok bool

		commit, ok = data.tagCommits[tag]
		if !ok {
			return nil, fmt.Errorf("%w: %q in %s", errTagNotFoundInRepo, tag, ref.RepoURL)
		}
	}

	var major, minor, patch uint64

	var rawVersion string

	v, err := semver.NewVersion(tag)
	if err == nil {
		major = v.Major()
		minor = v.Minor()
		patch = v.Patch()
		rawVersion = fmt.Sprintf("%d.%d.%d", major, minor, patch)
	}

	return &ActionReference{
		GitURL:     ref.RepoURL,
		Tag:        tag,
		FullCommit: commit,
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		RawVersion: rawVersion,
	}, nil
}

// findLatestSemverTag returns the tag name with the highest semantic version.
// Falls back to the first tag when none are valid semver.
func findLatestSemverTag(tagNames []string) (string, error) {
	type semverTag struct {
		version  *semver.Version
		original string
	}

	var semverTags []semverTag

	for _, t := range tagNames {
		v, err := semver.NewVersion(t)
		if err != nil {
			log.Debug("Skipping non-semver tag", "tag", t)

			continue
		}

		semverTags = append(semverTags, semverTag{version: v, original: t})
	}

	if len(semverTags) == 0 {
		return "", errNoValidSemverTags
	}

	sort.Slice(semverTags, func(i, j int) bool {
		return semverTags[i].version.GreaterThan(semverTags[j].version)
	})

	return semverTags[0].original, nil
}

// getHTTPAuth creates HTTP authentication using .netrc credentials.
//
//nolint:nilnil
func (up *Updater) getHTTPAuth(repoURL string) (transport.AuthMethod, error) {
	// Parse the URL to get the hostname
	u, err := url.Parse(repoURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse repository URL %s: %w", repoURL, err)
	}

	// Try to load .netrc file
	netrcFile, err := netrc.Parse(up.config.NetrcPath)
	if os.IsNotExist(err) {
		// .netrc file doesn't exist
		log.Debug("No .netrc file found", "path", up.config.NetrcPath)

		return nil, nil
	}

	if err != nil {
		log.Warn("Failed to parse .netrc", "error", err)

		return nil, nil
	}

	// Look for credentials for this hostname
	machine := netrcFile.Machine(u.Hostname())
	if machine == nil {
		// No credentials found for this hostname
		log.Debug("No credentials found in .netrc", "hostname", u.Hostname())

		return nil, nil
	}

	log.Debug("Using HTTP basic auth", "hostname", u.Hostname(), "user", machine.Get("login"))

	return &http.BasicAuth{
		Username: machine.Get("login"),
		Password: machine.Get("password"),
	}, nil
}

// getAuthMethod returns the appropriate authentication method for a repository URL.
func (up *Updater) getAuthMethod(repoURL string) (transport.AuthMethod, error) {
	if up.config.UseSSH || strings.HasPrefix(repoURL, "git@") || strings.HasPrefix(repoURL, "ssh://") {
		return up.getSSHAuth()
	}

	return up.getHTTPAuth(repoURL)
}

// getSSHAuth creates SSH authentication using SSH agent.
func (up *Updater) getSSHAuth() (transport.AuthMethod, error) {
	log.Debug("Using SSH agent authentication")

	sshAgent, err := ssh.NewSSHAgentAuth("git")
	if err != nil {
		return nil, fmt.Errorf("failed to connect to SSH agent (make sure ssh-agent is running and has keys loaded): %w", err)
	}

	// Set up known hosts verification
	homeDir, _ := os.UserHomeDir()
	knownHostsPath := filepath.Join(homeDir, ".ssh", "known_hosts")

	_, err = os.Stat(knownHostsPath)
	if err == nil {
		callback, err := knownhosts.New(knownHostsPath)
		if err != nil {
			log.Warn("Failed to load known_hosts", "error", err)
		} else {
			sshAgent.HostKeyCallback = callback
		}
	}

	return sshAgent, nil
}
