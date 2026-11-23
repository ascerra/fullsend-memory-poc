package repos

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge"
)

// repoScaffoldCommit records the repositories that received a scaffold
// commit.
type repoScaffoldCommit struct {
	mu    sync.Mutex
	repos []string
}

func (s *repoScaffoldCommit) fn() ScaffoldCommitFunc {
	return func(_ context.Context, owner, repo string, _ []forge.TreeFile, _ bool, _ bool) error {
		s.mu.Lock()
		s.repos = append(s.repos, owner+"/"+repo)
		s.mu.Unlock()
		return nil
	}
}

// progressRecorder records progress messages per repository and phase.
type progressRecorder struct {
	mu       sync.Mutex
	messages map[string][]string
}

func (p *progressRecorder) fn() ProgressFunc {
	return func(repo, phase, msg string) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.messages == nil {
			p.messages = make(map[string][]string)
		}
		p.messages[repo+"|"+phase] = append(p.messages[repo+"|"+phase], msg)
	}
}

func (p *progressRecorder) get(repo, phase string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.messages[repo+"|"+phase]
}

// writesTo reports whether fc recorded any secret or variable write for
// owner/repo.
func writesTo(fc *forge.FakeClient, fullName string) bool {
	for _, s := range fc.CreatedSecrets {
		if s.Owner+"/"+s.Repo == fullName {
			return true
		}
	}
	for key := range fc.VariableValues {
		if strings.HasPrefix(key, fullName+"/") {
			return true
		}
	}
	return false
}

// addListedRepo makes owner/name appear in later org listings, as when it
// is created while install runs.
func addListedRepo(fc *forge.FakeClient, fullName string) {
	owner, name, _ := strings.Cut(fullName, "/")
	fc.PipelineVarOverrideRoles[fullName] = forge.PipelineVarOverrideNoOneAllowed
	fc.Repos = append(fc.Repos, forge.Repository{FullName: owner + "/" + name, Name: name, DefaultBranch: "main"})
}

// A repository a glob matches only in a listing made after the
// configuration preflights passed none of them: Converge skips it with a
// warning and converges the evaluated repositories (#18).
func TestConverge_PreflightListingSkipsRepoCreatedAfterPreflights(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "dry-run"}[dryRun], func(t *testing.T) {
			ctx := context.Background()
			fc := newFakeClientForBatch("acme/api")
			m := newConvergeManifest("acme/*")
			clients := newTestClientFactory(fc)

			preflight := NewPreflightReadCache(clients)
			evaluated, err := selectManifestRepos(ctx, m, preflight, nil)
			require.NoError(t, err)
			require.Len(t, evaluated, 1)

			addListedRepo(fc, "acme/web")

			cfg := convergeCfgWithDefaults(m)
			cfg.DryRun = dryRun
			cfg.PreflightListing = preflight
			sc := &repoScaffoldCommit{}
			progress := &progressRecorder{}

			result, err := Converge(ctx, cfg, clients, sc.fn(), progress.fn())
			require.NoError(t, err)

			require.Len(t, result.Results, 1)
			assert.Equal(t, "acme/api", result.Results[0].Owner+"/"+result.Results[0].Repo)
			assert.NoError(t, result.Results[0].Error)
			assert.NotContains(t, sc.repos, "acme/web")
			assert.False(t, writesTo(fc, "acme/web"), "a repo the preflights did not evaluate takes no write")
			if !dryRun {
				assert.Equal(t, []string{"acme/api"}, sc.repos)
				assert.True(t, writesTo(fc, "acme/api"))
			}

			warnings := progress.get("acme/web", "warning")
			require.Len(t, warnings, 1)
			assert.Contains(t, warnings[0], "skipped")
			assert.Contains(t, warnings[0], "Run repos install again")
		})
	}
}

// A repo filter naming a glob-covered repository the preflight listing did
// not contain still selects it, as the preflights did, so it is converged.
func TestConverge_PreflightListingKeepsFilterNamedGlobRepo(t *testing.T) {
	ctx := context.Background()
	fc := newFakeClientForBatch("acme/api")
	m := newConvergeManifest("acme/*")
	clients := newTestClientFactory(fc)

	preflight := NewPreflightReadCache(clients)
	filter := []string{"acme/web"}
	evaluated, err := selectManifestRepos(ctx, m, preflight, filter)
	require.NoError(t, err)
	require.Len(t, evaluated, 1, "the preflights select a filter-named glob-covered repo")

	addListedRepo(fc, "acme/web")

	cfg := convergeCfgWithDefaults(m)
	cfg.RepoFilter = filter
	cfg.PreflightListing = preflight
	sc := &repoScaffoldCommit{}
	progress := &progressRecorder{}

	result, err := Converge(ctx, cfg, clients, sc.fn(), progress.fn())
	require.NoError(t, err)
	require.Len(t, result.Results, 1)
	assert.Equal(t, "acme/web", result.Results[0].Owner+"/"+result.Results[0].Repo)
	assert.Empty(t, progress.get("acme/web", "warning"))
}

// A repository that disappears after the preflights is not converged, and
// --exclude-repos still applies to the evaluated ones.
func TestConverge_PreflightListingWithRemovedAndExcludedRepos(t *testing.T) {
	ctx := context.Background()
	fc := newFakeClientForBatch("acme/api", "acme/gone", "acme/web")
	m := newConvergeManifest("acme/*")
	clients := newTestClientFactory(fc)

	preflight := NewPreflightReadCache(clients)
	_, err := selectManifestRepos(ctx, m, preflight, nil)
	require.NoError(t, err)

	kept := fc.Repos[:0:0]
	for _, r := range fc.Repos {
		if r.Name != "gone" {
			kept = append(kept, r)
		}
	}
	fc.Repos = kept

	cfg := convergeCfgWithDefaults(m)
	cfg.ExcludeRepos = []string{"acme/web"}
	cfg.PreflightListing = preflight
	sc := &repoScaffoldCommit{}

	result, err := Converge(ctx, cfg, clients, sc.fn(), noopProgress)
	require.NoError(t, err)
	require.Len(t, result.Results, 1)
	assert.Equal(t, "acme/api", result.Results[0].Owner+"/"+result.Results[0].Repo)
	assert.Equal(t, []string{"acme/api"}, sc.repos)
}

// Without a preflight listing, Converge expands globs from the live listing
// alone, as callers other than repos install expect.
func TestConverge_NoPreflightListingUsesLiveListing(t *testing.T) {
	ctx := context.Background()
	fc := newFakeClientForBatch("acme/api")
	m := newConvergeManifest("acme/*")
	addListedRepo(fc, "acme/web")

	result, err := Converge(ctx, convergeCfgWithDefaults(m), newTestClientFactory(fc), (&repoScaffoldCommit{}).fn(), noopProgress)
	require.NoError(t, err)
	assert.Len(t, result.Results, 2)
}

// A preflight listing that cannot be read fails convergence before any
// write.
func TestConverge_PreflightListingErrorFailsBeforeWrites(t *testing.T) {
	ctx := context.Background()
	fc := newFakeClientForBatch("acme/api")
	m := newConvergeManifest("acme/*")

	cfg := convergeCfgWithDefaults(m)
	cfg.PreflightListing = newTestClientFactory(failingOrgClient{FakeClient: fc, failOrg: "acme"})
	sc := &repoScaffoldCommit{}

	_, err := Converge(ctx, cfg, newTestClientFactory(fc), sc.fn(), noopProgress)
	require.ErrorContains(t, err, "expanding globs from the preflight listing")
	assert.Empty(t, sc.repos)
	assert.False(t, writesTo(fc, "acme/api"))
}
