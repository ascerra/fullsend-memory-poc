package repos

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fullsend-ai/fullsend/internal/forge"
)

// deleteServerDetail is echoed by failingDeleteClient errors so tests can
// check that redacted secret names withhold server text.
const deleteServerDetail = "server-detail-echo-42"

// failingDeleteClient fails DeleteRepoVariable / DeleteRepoSecret for the
// listed names and records every delete call it receives.
type failingDeleteClient struct {
	*forge.FakeClient
	failVars    map[string]bool
	failSecrets map[string]bool

	mu             sync.Mutex
	varCalls       []string
	secretCalls    []string
	secretFailures int
}

func (c *failingDeleteClient) DeleteRepoVariable(ctx context.Context, owner, repo, name string) error {
	c.mu.Lock()
	c.varCalls = append(c.varCalls, name)
	c.mu.Unlock()
	if c.failVars[name] {
		return errors.New("500 from server: " + deleteServerDetail)
	}
	return c.FakeClient.DeleteRepoVariable(ctx, owner, repo, name)
}

func (c *failingDeleteClient) DeleteRepoSecret(ctx context.Context, owner, repo, name string) error {
	c.mu.Lock()
	c.secretCalls = append(c.secretCalls, name)
	c.mu.Unlock()
	if c.failSecrets[name] {
		return errors.New("500 from server: " + deleteServerDetail)
	}
	return c.FakeClient.DeleteRepoSecret(ctx, owner, repo, name)
}

func isRedactedUninstallSecret(name string) bool {
	return name == forge.SecretTriggerToken || name == forge.SecretWebhookSecret || name == forge.SecretTraceSigningKey
}

// uninstallLoopVars returns the variable names the uninstall delete loop
// handles for a forge; GitLab role identity variables are deleted by the
// role cleanup instead.
func uninstallLoopVars(forgeName string) []string {
	var names []string
	for _, name := range UninstallVarsForForge(forgeName) {
		if forgeName == ForgeGitLab && isGitLabIdentityUninstallVar(name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// A failed variable or secret delete does not stop uninstall from
// attempting every remaining name, and each failure is reported.
func TestUninstall_FailedDeleteAttemptsRemainingNames(t *testing.T) {
	pick := func(names []string, positions ...string) []string {
		var out []string
		for _, p := range positions {
			switch p {
			case "first":
				out = append(out, names[0])
			case "middle":
				out = append(out, names[len(names)/2])
			case "last":
				out = append(out, names[len(names)-1])
			}
		}
		return out
	}
	positionSets := [][]string{{"first"}, {"middle"}, {"last"}, {"first", "last"}}

	for _, forgeName := range []string{ForgeGitHub, ForgeGitLab} {
		for _, kind := range []string{"variable", "secret"} {
			for _, positions := range positionSets {
				name := forgeName + "/" + kind + "/" + strings.Join(positions, "+")
				t.Run(name, func(t *testing.T) {
					var fake *forge.FakeClient
					var manifest *Manifest
					if forgeName == ForgeGitLab {
						fake = newInstalledFakeGitLabClient("acme/api")
						manifest = testGitLabManifest("acme/api")
					} else {
						fake = newInstalledFakeClient("acme/api")
						manifest = testManifest("acme/api")
					}
					vars := uninstallLoopVars(forgeName)
					secrets := UninstallSecretsForForge(forgeName)

					client := &failingDeleteClient{FakeClient: fake, failVars: map[string]bool{}, failSecrets: map[string]bool{}}
					var failing []string
					if kind == "variable" {
						failing = pick(vars, positions...)
						for _, n := range failing {
							client.failVars[n] = true
						}
					} else {
						failing = pick(secrets, positions...)
						for _, n := range failing {
							client.failSecrets[n] = true
						}
					}

					var progressLines []string
					var progressMu sync.Mutex
					results, err := Uninstall(context.Background(), UninstallConfig{
						Manifest:       manifest,
						Repos:          []string{"acme/api"},
						Direct:         true,
						MaxConcurrency: 4,
					}, newTestClientFactory(client), uninstallCommitFn(fake), func(_, _, message string) {
						progressMu.Lock()
						progressLines = append(progressLines, message)
						progressMu.Unlock()
					})
					require.NoError(t, err)
					require.Len(t, results, 1)
					r := results[0]

					assert.False(t, r.Success, "a failed delete must leave the repository reported as failed")
					require.Error(t, r.Error)

					for _, n := range vars {
						assert.Contains(t, client.varCalls, n, "variable %s was not attempted", n)
					}
					for _, n := range secrets {
						assert.Contains(t, client.secretCalls, n, "secret %s was not attempted", n)
					}

					msg := r.Error.Error()
					for _, n := range failing {
						assert.Contains(t, msg, "deleting "+kind+" "+n)
					}

					if kind == "secret" {
						assert.Equal(t, len(secrets)-len(failing), r.SecretsDeleted)
						// Each failure is reported on its own line; the line
						// for a redacted secret must withhold the server text.
						for _, n := range failing {
							if !isRedactedUninstallSecret(n) {
								continue
							}
							for line := range strings.SplitSeq(msg, "\n") {
								if strings.Contains(line, "deleting secret "+n) {
									assert.NotContains(t, line, deleteServerDetail, "server text for %s must be withheld", n)
								}
							}
							for _, progress := range progressLines {
								for line := range strings.SplitSeq(progress, "\n") {
									if strings.Contains(line, "deleting secret "+n) {
										assert.NotContains(t, line, deleteServerDetail)
									}
								}
							}
						}
					} else {
						assert.Equal(t, len(secrets), r.SecretsDeleted)
						if forgeName == ForgeGitHub {
							assert.Equal(t, len(vars)-len(failing), r.VarsDeleted)
						}
					}
				})
			}
		}
	}
}
