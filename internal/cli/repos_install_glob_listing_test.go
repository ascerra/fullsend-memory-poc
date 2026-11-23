package cli

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge"
)

// growingOrgClient hides the repositories named in late from the first
// hidden org listings it serves, as when they are created while install
// runs.
type growingOrgClient struct {
	*forge.FakeClient
	late   map[string]bool
	hidden int

	mu       sync.Mutex
	listings int
}

func (c *growingOrgClient) ListOrgRepos(ctx context.Context, org string, includePrivate bool) ([]forge.Repository, error) {
	listed, err := c.FakeClient.ListOrgRepos(ctx, org, includePrivate)
	if err != nil {
		return listed, err
	}
	c.mu.Lock()
	c.listings++
	reveal := c.listings > c.hidden
	c.mu.Unlock()
	if reveal {
		return listed, nil
	}
	var visible []forge.Repository
	for _, r := range listed {
		if !c.late[r.FullName] {
			visible = append(visible, r)
		}
	}
	return visible, nil
}

// A repository a glob matches only in an org listing made after the
// configuration preflights is not converged: it passed none of them and has
// no trace signing key plan. Install converges the repositories the
// preflights evaluated, writes their trace signing keys, and reports the
// late repository as skipped (#18).
func TestRunReposInstall_RepoCreatedAfterPreflightsIsSkipped(t *testing.T) {
	globManifest := strings.Replace(githubUnmanagedManifestYAML, "acme/api", "acme/*", 1)
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "dry-run"}[dryRun], func(t *testing.T) {
			manifestPath := writeTestManifest(t, globManifest)
			fc := newInstallFakeClient("acme/api", "acme/web")
			// The GitLab trigger-account selection lists the org first, then
			// the configuration preflights list it once through their cache.
			client := &growingOrgClient{FakeClient: fc, late: map[string]bool{"acme/web": true}, hidden: 2}
			opts := githubManagedInstallOpts(manifestPath, fc)
			opts.testClient = client
			opts.dryRun = dryRun

			var err error
			out := captureStdout(t, func() { err = runReposInstall(context.Background(), opts) })
			require.NoError(t, err, out)
			require.Greater(t, client.listings, client.hidden, "convergence lists the org again")
			require.NotContains(t, out, "[acme/web] Persisted", "the preflights did not see the late repository")
			require.NotContains(t, out, "[acme/web] Would persist", "the preflights did not see the late repository")

			commits, secrets, variables := repoWrites(fc, "acme/web")
			assert.Zero(t, commits+secrets+variables, "the late repository takes no write")
			assert.NotContains(t, manifestEntryYAML(t, manifestPath, "acme/web"), "trace_context")
			assert.NotContains(t, out, "[acme/web] Would install")
			assert.Contains(t, out, "[acme/web] skipped: this repository appeared after the configuration preflights")
			assert.Contains(t, out, "Run repos install again to install it")

			if dryRun {
				assert.Equal(t, globManifest, mustReadFile(t, manifestPath), "a dry run leaves repos.yaml byte-identical")
				assert.Contains(t, out, "[acme/api] Would install")
				return
			}
			requireTraceKeyWritten(t, fc, "acme/api", out)
			commits, _, _ = repoWrites(fc, "acme/api")
			assert.Positive(t, commits, "the evaluated repository is installed")
		})
	}
}
