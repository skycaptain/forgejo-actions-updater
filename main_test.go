package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jdx/go-netrc"
)

const testRepoURL = "https://example.com/org/repo"

func TestFindActionReferencesParsing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  *ParsedActionReference
	}{
		{
			name:  "full URL with SHA and comment",
			input: "      - uses: https://code.forgejo.org/forgejo/checkout@abc1234def5678  # v3.0.0",
			want: &ParsedActionReference{
				RepoURL: "https://code.forgejo.org/forgejo/checkout",
				Ref:     "abc1234def5678",
				Comment: "v3.0.0",
			},
		},
		{
			name:  "full URL with tag, no comment",
			input: "      - uses: https://code.forgejo.org/forgejo/checkout@v3.0.0",
			want: &ParsedActionReference{
				RepoURL: "https://code.forgejo.org/forgejo/checkout",
				Ref:     "v3.0.0",
				Comment: "",
			},
		},
		{
			name:  "short form with SHA and comment",
			input: "      - uses: actions/checkout@abc1234def5678  # v4.1.0",
			want: &ParsedActionReference{
				RepoURL: "https://github.com/actions/checkout",
				Ref:     "abc1234def5678",
				Comment: "v4.1.0",
			},
		},
		{
			name:  "short form with tag",
			input: "      - uses: actions/checkout@v4.1.0",
			want: &ParsedActionReference{
				RepoURL: "https://github.com/actions/checkout",
				Ref:     "v4.1.0",
				Comment: "",
			},
		},
		{
			name:  "invalid - no @ separator",
			input: "      - uses: actions/checkout",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var refs []*ParsedActionReference
			for _, m := range actionRefRe.FindAllStringSubmatch(tt.input, -1) {
				refs = append(refs, parseActionMatch(m))
			}

			if tt.want == nil {
				if len(refs) != 0 {
					t.Errorf("Expected no refs, got %+v", refs)
				}

				return
			}

			if len(refs) != 1 {
				t.Fatalf("Expected 1 ref, got %d", len(refs))
			}

			result := refs[0]

			if result.RepoURL != tt.want.RepoURL {
				t.Errorf("RepoURL: expected %q, got %q", tt.want.RepoURL, result.RepoURL)
			}

			if result.Ref != tt.want.Ref {
				t.Errorf("Ref: expected %q, got %q", tt.want.Ref, result.Ref)
			}

			if result.Comment != tt.want.Comment {
				t.Errorf("Comment: expected %q, got %q", tt.want.Comment, result.Comment)
			}
		})
	}
}

func TestStringReplacement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		oldFrag string
		newFrag string
		want    string
	}{
		{
			name:    "replace locked SHA with new SHA",
			content: "      - uses: https://github.com/actions/checkout@abc123  # v3.0.0\n",
			oldFrag: "uses: https://github.com/actions/checkout@abc123  # v3.0.0",
			newFrag: "uses: https://github.com/actions/checkout@def456  # v4.0.0",
			want:    "      - uses: https://github.com/actions/checkout@def456  # v4.0.0\n",
		},
		{
			name:    "replace tag reference with locked SHA",
			content: "        uses: actions/setup-go@v5\n",
			oldFrag: "uses: actions/setup-go@v5",
			newFrag: "uses: https://github.com/actions/setup-go@aabbccdd  # v5.0.2",
			want:    "        uses: https://github.com/actions/setup-go@aabbccdd  # v5.0.2\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := strings.Replace(tt.content, tt.oldFrag, tt.newFrag, 1)
			if result != tt.want {
				t.Errorf("Expected %q, got %q", tt.want, result)
			}
		})
	}
}

//nolint:paralleltest // test using t.Setenv, t.Chdir, or cryptotest.SetGlobalRandom can not use t.Parallel
func TestFindForgejoWorkflows(t *testing.T) {
	tempDir := t.TempDir()

	// Place a .git marker so findGitRoot resolves tempDir as the project root.
	err := os.MkdirAll(filepath.Join(tempDir, ".git"), 0o755)
	if err != nil {
		t.Fatalf("Failed to create .git dir: %v", err)
	}

	workflowsDir := filepath.Join(tempDir, ".forgejo", "workflows")

	err = os.MkdirAll(workflowsDir, 0o755)
	if err != nil {
		t.Fatalf("Failed to create workflows dir: %v", err)
	}

	// Create matching workflow files
	for _, name := range []string{"ci.yaml", "release.yml"} {
		err := os.WriteFile(filepath.Join(workflowsDir, name), []byte("on: push\n"), 0o644)
		if err != nil {
			t.Fatalf("Failed to create %s: %v", name, err)
		}
	}

	// Create non-matching files (should be ignored)
	err = os.WriteFile(filepath.Join(workflowsDir, "README.md"), []byte("# docs"), 0o644)
	if err != nil {
		t.Fatalf("Failed to create README.md: %v", err)
	}

	err = os.WriteFile(filepath.Join(tempDir, "other.yaml"), []byte("other"), 0o644)
	if err != nil {
		t.Fatalf("Failed to create other.yaml: %v", err)
	}

	updater := &Updater{}

	// Explicit path: pass the root directly — no git-root traversal.
	found, err := updater.findForgejoWorkflows(tempDir)
	if err != nil {
		t.Fatalf("findForgejoWorkflows (explicit) failed: %v", err)
	}

	if len(found) != 2 {
		t.Errorf("Expected 2 files, got %d: %v", len(found), found)
	}

	for _, f := range found {
		if !strings.HasSuffix(f, ".yaml") && !strings.HasSuffix(f, ".yml") {
			t.Errorf("Unexpected file in results: %s", f)
		}
	}

	// Auto-detect: chdir into a subdirectory and pass an empty path.
	// findForgejoWorkflows should walk up to the git root and find the workflows.
	subDir := filepath.Join(tempDir, "pkg", "util")

	err = os.MkdirAll(subDir, 0o755)
	if err != nil {
		t.Fatalf("Failed to create subdir: %v", err)
	}

	t.Chdir(subDir)

	found2, err := updater.findForgejoWorkflows("")
	if err != nil {
		t.Fatalf("findForgejoWorkflows (auto-detect from subdir) failed: %v", err)
	}

	if len(found2) != 2 {
		t.Errorf("Expected 2 files from auto-detect, got %d: %v", len(found2), found2)
	}
}

func TestFindForgejoWorkflowsMissingDir(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()

	// .git marker so findGitRoot resolves without error
	err := os.MkdirAll(filepath.Join(tempDir, ".git"), 0o755)
	if err != nil {
		t.Fatalf("Failed to create .git dir: %v", err)
	}

	updater := &Updater{}

	found, err := updater.findForgejoWorkflows(tempDir)
	if err != nil {
		t.Fatalf("Expected no error for missing workflows dir, got: %v", err)
	}

	if found != nil {
		t.Errorf("Expected nil slice for missing dir, got: %v", found)
	}
}

func TestFindActionReferences(t *testing.T) {
	t.Parallel()
	// editorconfig-checker-disable
	content := `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@abc123  # v4.1.0
      - uses: https://code.forgejo.org/forgejo/setup-go@def456  # v5.0.0
      - name: Manual step
        run: echo hello
      - uses: actions/upload-artifact@v4
`
	// editorconfig-checker-enable

	matches := actionRefRe.FindAllStringSubmatch(content, -1)

	refs := make([]*ParsedActionReference, 0, len(matches))
	for _, m := range matches {
		refs = append(refs, parseActionMatch(m))
	}

	if len(refs) != 3 {
		t.Fatalf("Expected 3 action references, got %d", len(refs))
	}

	if refs[0].RepoURL != "https://github.com/actions/checkout" {
		t.Errorf("ref[0] RepoURL: expected https://github.com/actions/checkout, got %s", refs[0].RepoURL)
	}

	if refs[0].Comment != "v4.1.0" {
		t.Errorf("ref[0] Comment: expected v4.1.0, got %s", refs[0].Comment)
	}

	if refs[1].RepoURL != "https://code.forgejo.org/forgejo/setup-go" {
		t.Errorf("ref[1] RepoURL: expected https://code.forgejo.org/forgejo/setup-go, got %s", refs[1].RepoURL)
	}

	if refs[2].Ref != "v4" {
		t.Errorf("ref[2] Ref: expected v4, got %s", refs[2].Ref)
	}
}

func TestNewUpdater(t *testing.T) {
	t.Parallel()

	cfg := &Config{DryRun: true, Bump: true, Lock: true}
	up := NewUpdater(cfg)

	if up == nil {
		t.Fatal("NewUpdater returned nil")
	}

	if up.config != cfg {
		t.Error("NewUpdater did not store the config")
	}

	if up.cache == nil {
		t.Error("NewUpdater did not initialize the cache")
	}
}

func TestFindLatestSemverTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tags    []string
		want    string
		wantErr bool
	}{
		{
			name: "picks highest semver",
			tags: []string{"v1.0.0", "v2.0.0", "v1.5.0"},
			want: "v2.0.0",
		},
		{
			name: "semver without v prefix",
			tags: []string{"1.0.0", "2.0.0", "1.5.0"},
			want: "2.0.0",
		},
		{
			name: "mixed semver and non-semver",
			tags: []string{"v1.0.0", "latest", "v1.1.0"},
			want: "v1.1.0",
		},
		{
			name:    "no valid semver - returns error",
			tags:    []string{"alpha", "beta", "stable"},
			wantErr: true,
		},
		{
			name: "single tag",
			tags: []string{"v3.0.0"},
			want: "v3.0.0",
		},
		{
			name: "release beats prerelease",
			tags: []string{"v1.0.0-rc1", "v1.0.0"},
			want: "v1.0.0",
		},
		{
			name: "patch beats older minor",
			tags: []string{"v1.2.0", "v1.2.3", "v1.2.1"},
			want: "v1.2.3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result, err := findLatestSemverTag(tt.tags)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Expected error, got nil")
				}

				return
			}

			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}

			if result != tt.want {
				t.Errorf("Expected %q, got %q", tt.want, result)
			}
		})
	}
}

func TestUpdateFileNotFound(t *testing.T) {
	t.Parallel()

	up := NewUpdater(&Config{Bump: true, Lock: true})

	err := up.updateFile("/nonexistent/path/file.yaml")
	if err == nil {
		t.Error("Expected error for non-existent file, got nil")
	}
}

func TestUpdateFileNoActionRefs(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "ci.yaml")

	// editorconfig-checker-disable
	content := `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: echo hello
`
	// editorconfig-checker-enable

	err := os.WriteFile(filePath, []byte(content), 0o644)
	if err != nil {
		t.Fatalf("Failed to create file: %v", err)
	}

	up := NewUpdater(&Config{Bump: true, Lock: true})

	err = up.updateFile(filePath)
	if err != nil {
		t.Errorf("Expected no error for file without action refs, got: %v", err)
	}
}

func TestUpdateWorkflowsNoFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	up := NewUpdater(&Config{Bump: true, Lock: true, NetrcPath: "/nonexistent"})

	err := up.UpdateWorkflows(tmpDir)
	if err != nil {
		t.Errorf("Expected no error when no workflow files exist, got: %v", err)
	}
}

func TestUpdateWorkflowsWithNoRefFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	workflowsDir := filepath.Join(tmpDir, ".forgejo", "workflows")

	err := os.MkdirAll(workflowsDir, 0o755)
	if err != nil {
		t.Fatalf("Failed to create workflows dir: %v", err)
	}

	// editorconfig-checker-disable
	content := `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
`
	// editorconfig-checker-enable

	err = os.WriteFile(filepath.Join(workflowsDir, "ci.yaml"), []byte(content), 0o644)
	if err != nil {
		t.Fatalf("Failed to create workflow file: %v", err)
	}

	up := NewUpdater(&Config{Bump: true, Lock: true, NetrcPath: "/nonexistent"})

	err = up.UpdateWorkflows(tmpDir)
	if err != nil {
		t.Errorf("Expected no error, got: %v", err)
	}
}

func TestFindGitRootNoRepo(t *testing.T) {
	t.Parallel()

	// t.TempDir() lives under the OS temp dir which has no .git above it.
	tmpDir := t.TempDir()

	_, err := findGitRoot(tmpDir)
	if err == nil {
		t.Error("Expected error when no git repository found, got nil")
	}
}

func TestResolveActionConnectionError(t *testing.T) {
	t.Parallel()

	// Port 59999 on loopback should be refused immediately — no DNS, no wait.
	ref := &ParsedActionReference{
		RepoURL: "http://127.0.0.1:59999/org/repo",
		Ref:     "v1.0.0",
	}
	up := NewUpdater(&Config{Bump: true, Lock: true, NetrcPath: "/nonexistent"})

	_, err := up.resolveAction(ref)
	if err == nil {
		t.Error("Expected connection error, got nil")
	}
}

func TestFetchRemoteDataCachingError(t *testing.T) {
	t.Parallel()
	// Port 59999 on loopback should be refused immediately.
	repoURL := "http://127.0.0.1:59999/org/repo"
	up := NewUpdater(&Config{Bump: true, Lock: true, NetrcPath: "/nonexistent"})

	_, err1 := up.fetchRemoteData(repoURL)
	if err1 == nil {
		t.Fatal("Expected connection error on first call, got nil")
	}

	// A failed fetch must not populate the cache.
	if _, ok := up.cache[repoURL]; ok {
		t.Error("Cache must not be populated after a failed fetch")
	}

	// Second call must also hit the network, not a stale nil entry.
	_, err2 := up.fetchRemoteData(repoURL)
	if err2 == nil {
		t.Fatal("Expected connection error on second call, got nil")
	}
}

func TestFetchRemoteDataCacheHit(t *testing.T) {
	t.Parallel()

	up := NewUpdater(&Config{})
	repoURL := testRepoURL

	want := &remoteData{
		tagCommits: map[string]string{"v1.0.0": "abc123"},
		tagNames:   []string{"v1.0.0"},
	}
	up.cache[repoURL] = want

	got, err := up.fetchRemoteData(repoURL)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if got != want {
		t.Error("Expected cache hit to return the pre-populated value")
	}
}

func TestResolveActionBumpAndLock(t *testing.T) {
	t.Parallel()

	up := NewUpdater(&Config{Bump: true, Lock: true})
	repoURL := testRepoURL
	up.cache[repoURL] = &remoteData{
		tagCommits: map[string]string{"v1.0.0": "sha1", "v2.0.0": "sha2"},
		tagNames:   []string{"v1.0.0", "v2.0.0"},
	}

	ref := &ParsedActionReference{RepoURL: repoURL, Ref: "sha1", Comment: "v1.0.0"}

	result, err := up.resolveAction(ref)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Tag != "v2.0.0" {
		t.Errorf("Expected tag v2.0.0, got %s", result.Tag)
	}

	if result.FullCommit != "sha2" {
		t.Errorf("Expected sha sha2, got %s", result.FullCommit)
	}

	if result.RawVersion != "2.0.0" {
		t.Errorf("Expected RawVersion 2.0.0, got %s", result.RawVersion)
	}

	if result.Major != 2 || result.Minor != 0 || result.Patch != 0 {
		t.Errorf("Expected Major=2 Minor=0 Patch=0, got %d.%d.%d", result.Major, result.Minor, result.Patch)
	}
}

func TestResolveActionNoBumpByComment(t *testing.T) {
	t.Parallel()

	up := NewUpdater(&Config{Bump: false, Lock: true})
	repoURL := testRepoURL
	up.cache[repoURL] = &remoteData{
		tagCommits: map[string]string{"v1.5.0": "sha15"},
		tagNames:   []string{"v1.5.0"},
	}

	ref := &ParsedActionReference{RepoURL: repoURL, Ref: "deadbeef", Comment: "v1.5.0"}

	result, err := up.resolveAction(ref)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Tag != "v1.5.0" {
		t.Errorf("Expected tag v1.5.0, got %s", result.Tag)
	}

	if result.FullCommit != "sha15" {
		t.Errorf("Expected sha sha15, got %s", result.FullCommit)
	}
}

func TestResolveActionNoBumpByRef(t *testing.T) {
	t.Parallel()

	up := NewUpdater(&Config{Bump: false, Lock: false})
	repoURL := testRepoURL
	up.cache[repoURL] = &remoteData{
		tagCommits: map[string]string{"v1.0.0": "sha1"},
		tagNames:   []string{"v1.0.0"},
	}

	// No comment: tag falls back to current Ref.
	ref := &ParsedActionReference{RepoURL: repoURL, Ref: "v1.0.0"}

	result, err := up.resolveAction(ref)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if result.Tag != "v1.0.0" {
		t.Errorf("Expected tag v1.0.0, got %s", result.Tag)
	}

	if result.FullCommit != "" {
		t.Errorf("Expected empty sha (Lock=false), got %s", result.FullCommit)
	}
}

func TestResolveActionTagNotFound(t *testing.T) {
	t.Parallel()

	up := NewUpdater(&Config{Bump: false, Lock: true})
	repoURL := testRepoURL
	up.cache[repoURL] = &remoteData{
		tagCommits: map[string]string{"v1.0.0": "sha1"},
		tagNames:   []string{"v1.0.0"},
	}

	// Comment refers to a tag absent from the cached tagSHAs.
	ref := &ParsedActionReference{RepoURL: repoURL, Ref: "sha1", Comment: "v9.9.9"}

	_, err := up.resolveAction(ref)
	if err == nil {
		t.Error("Expected tag-not-found error, got nil")
	}
}

func TestUpdateFileFullUpdate(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "ci.yaml")

	// editorconfig-checker-disable
	content := `name: CI
on: push
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@oldsha  # v1.0.0
`
	// editorconfig-checker-enable

	err := os.WriteFile(filePath, []byte(content), 0o644)
	if err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	up := NewUpdater(&Config{Bump: true, Lock: true, Template: actionRefTmpl})
	up.cache["https://github.com/actions/checkout"] = &remoteData{
		tagCommits: map[string]string{"v1.0.0": "oldsha", "v2.0.0": "newsha"},
		tagNames:   []string{"v1.0.0", "v2.0.0"},
	}

	err = up.updateFile(filePath)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("Failed to read updated file: %v", err)
	}

	if !strings.Contains(string(got), "uses: https://github.com/actions/checkout@newsha  # v2.0.0") {
		t.Errorf("Expected updated action ref in file, got:\n%s", string(got))
	}
}

func TestUpdateFileAlreadyUpToDate(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "ci.yaml")

	// editorconfig-checker-disable
	content := `name: CI
on: push
jobs:
  build:
    steps:
      - uses: https://github.com/actions/checkout@sha123  # v2.0.0
`
	// editorconfig-checker-enable

	err := os.WriteFile(filePath, []byte(content), 0o644)
	if err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	up := NewUpdater(&Config{Bump: true, Lock: true, Template: actionRefTmpl})
	up.cache["https://github.com/actions/checkout"] = &remoteData{
		tagCommits: map[string]string{"v2.0.0": "sha123"},
		tagNames:   []string{"v2.0.0"},
	}

	err = up.updateFile(filePath)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	got, _ := os.ReadFile(filePath)
	if string(got) != content {
		t.Errorf("File was modified when already up-to-date")
	}
}

func TestUpdateFileDryRun(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "ci.yaml")

	// editorconfig-checker-disable
	content := `name: CI
on: push
jobs:
  build:
    steps:
      - uses: actions/checkout@oldsha  # v1.0.0
`
	// editorconfig-checker-enable

	err := os.WriteFile(filePath, []byte(content), 0o644)
	if err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}

	up := NewUpdater(&Config{Bump: true, Lock: true, DryRun: true, Template: actionRefTmpl})
	up.cache["https://github.com/actions/checkout"] = &remoteData{
		tagCommits: map[string]string{"v1.0.0": "oldsha", "v2.0.0": "newsha"},
		tagNames:   []string{"v1.0.0", "v2.0.0"},
	}

	err = up.updateFile(filePath)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// File must not be modified in dry-run mode.
	got, _ := os.ReadFile(filePath)
	if string(got) != content {
		t.Errorf("File was modified in dry-run mode")
	}
}

func TestGetHTTPAuth(t *testing.T) {
	t.Parallel()

	// Create a temporary .netrc file for testing
	tmpDir := t.TempDir()
	netrcPath := filepath.Join(tmpDir, ".netrc")

	netrcContent := `machine code.forgejo.org
login testuser
password testtoken

machine github.com
login anotheruser
password anothertoken

# Comment line
machine example.com
login user3
password pass3`

	err := os.WriteFile(netrcPath, []byte(netrcContent), 0600)
	if err != nil {
		t.Fatalf("Failed to create test .netrc file: %v", err)
	}

	config := &Config{
		NetrcPath: netrcPath,
	}
	updater := &Updater{config: config}

	// Test parsing with go-netrc library directly
	netrcFile, err := netrc.Parse(netrcPath)
	if err != nil {
		t.Fatalf("Failed to parse .netrc with go-netrc: %v", err)
	}

	// Check that we can find machines
	forgejoMachine := netrcFile.Machine("code.forgejo.org")
	if forgejoMachine == nil {
		t.Error("Expected to find code.forgejo.org machine")
	} else {
		if forgejoMachine.Get("login") != "testuser" {
			t.Errorf("Expected login 'testuser', got '%s'", forgejoMachine.Get("login"))
		}

		if forgejoMachine.Get("password") != "testtoken" {
			t.Errorf("Expected password 'testtoken', got '%s'", forgejoMachine.Get("password"))
		}
	}

	githubMachine := netrcFile.Machine("github.com")
	if githubMachine == nil {
		t.Error("Expected to find github.com machine")
	} else if githubMachine.Get("login") != "anotheruser" {
		t.Errorf("Expected login 'anotheruser', got '%s'", githubMachine.Get("login"))
	}

	// Test getHTTPAuth method with matching hostname
	auth, err := updater.getHTTPAuth("https://code.forgejo.org/user/repo.git")
	if err != nil {
		t.Fatalf("Failed to get HTTP auth: %v", err)
	}

	if auth == nil {
		t.Error("Expected to get authentication, but got nil")
	}

	// Test with non-matching hostname
	auth, err = updater.getHTTPAuth("https://unknown.com/group/project.git")
	if err != nil {
		t.Fatalf("Failed to get HTTP auth: %v", err)
	}

	if auth != nil {
		t.Error("Expected no auth for unknown.com")
	}
}

func TestGetHTTPAuthMissingFile(t *testing.T) {
	t.Parallel()

	config := &Config{
		NetrcPath: "/nonexistent/.netrc",
	}
	updater := &Updater{config: config}

	auth, err := updater.getHTTPAuth("https://github.com/actions/checkout.git")
	if err != nil {
		t.Fatalf("Expected no error for missing .netrc file, got: %v", err)
	}

	if auth != nil {
		t.Error("Expected no auth for missing .netrc file")
	}
}

func TestGetAuthMethod(t *testing.T) {
	t.Parallel()

	config := &Config{
		UseSSH:    false,
		NetrcPath: "/nonexistent/.netrc",
	}
	updater := &Updater{config: config}

	// Test HTTPS URL (should try HTTP auth)
	auth, err := updater.getAuthMethod("https://github.com/actions/checkout.git")
	if err != nil {
		t.Fatalf("Expected no error for HTTPS URL, got: %v", err)
	}
	// Should be nil because .netrc doesn't exist
	if auth != nil {
		t.Error("Expected no auth when .netrc doesn't exist")
	}

	// Test SSH URL (should try SSH agent auth but fail because SSH agent not available in test)
	_, err = updater.getAuthMethod("git@github.com:actions/checkout.git")
	if err == nil {
		t.Log("SSH agent might be available in test environment")
	} else {
		t.Logf("Expected SSH agent error: %v", err)
	}
}

func TestGetSSHAuthAgent(t *testing.T) {
	t.Parallel()

	// Test SSH agent authentication
	config := &Config{
		UseSSH: true,
	}
	updater := &Updater{config: config}

	// This should try SSH agent directly
	auth, err := updater.getSSHAuth()
	if err != nil {
		// This is expected in test environment without SSH agent
		t.Logf("SSH auth failed as expected (no SSH agent in test): %v", err)

		return
	}

	if auth == nil {
		t.Error("Expected SSH auth to return authentication method or error")
	}
}
