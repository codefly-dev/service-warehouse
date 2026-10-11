package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type publisherStep struct {
	ID   string         `yaml:"id"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
}

type publisherJob struct {
	Uses        string            `yaml:"uses"`
	If          string            `yaml:"if"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []publisherStep   `yaml:"steps"`
}

func loadPublisher(t *testing.T) publisherJob {
	t.Helper()
	var workflow struct {
		Jobs map[string]publisherJob `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(workflowBody(t, "publish-gateway-image.yml")), &workflow))
	require.Len(t, workflow.Jobs, 1)
	return workflow.Jobs["publish"]
}

func publisherStepByID(t *testing.T, id string) publisherStep {
	t.Helper()
	for _, step := range loadPublisher(t).Steps {
		if step.ID == id {
			return step
		}
	}
	t.Fatalf("publisher has no %s step", id)
	return publisherStep{}
}

// GHCR checks the defining repo. A job-level reusable workflow call broke the
// live object-storage release even with a caller-supplied registry token.
func TestPublisherKeepsRegistryAuthorityInServiceRepository(t *testing.T) {
	job := loadPublisher(t)
	require.Empty(t, job.Uses, "the publisher job must be defined in this repository")
	require.Equal(t, "github.event_name == 'workflow_dispatch'", job.If)
	require.Equal(t, "write", job.Permissions["packages"])
	require.Equal(t, "read", job.Permissions["contents"])
	guard, login, build, artifact := -1, -1, -1, -1
	for i, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			require.Equal(t, false, step.With["persist-credentials"])
		}
		if step.ID == "default-branch" {
			guard = i
		}
		if strings.HasPrefix(step.Uses, "docker/login-action@") {
			login = i
			require.Equal(t, "${{ secrets.GITHUB_TOKEN }}", step.With["password"])
			require.Equal(t, "ghcr.io", step.With["registry"])
		}
		if strings.HasPrefix(step.Uses, "docker/build-push-action@") {
			build = i
			require.NotContains(t, step.With, "tags")
			require.Contains(t, step.With["outputs"], "push-by-digest=true")
			require.Contains(t, step.With["outputs"], "push=true")
			require.Contains(t, step.With["labels"], "org.opencontainers.image.version=${{ steps.inputs.outputs.version }}")
		}
		if strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
			artifact = i
			require.Equal(t, "release-evidence/gateway-image.json", step.With["path"])
			require.Equal(t, "error", step.With["if-no-files-found"])
		}
	}
	require.GreaterOrEqual(t, guard, 0)
	require.Greater(t, login, guard, "refuse unmerged code before registry login")
	require.Greater(t, build, login)
	require.Greater(t, artifact, build)
}

// Execute the workflow's actual guard against a local origin. The remote's
// default branch is deliberately not called main, and the supplied ref name
// lies: authority must come from the remote's HEAD and the commit's ancestry.
func TestPublisherRefusesCommitOutsideDefaultBranch(t *testing.T) {
	dir := t.TempDir()
	git := func(at string, args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = at
		out, err := command.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return strings.TrimSpace(string(out))
	}
	git(dir, "init", "--quiet", "--initial-branch=trunk")
	git(dir, "config", "commit.gpgsign", "false")
	git(dir, "config", "core.hooksPath", "/dev/null")
	git(dir, "config", "user.name", "publisher fixture")
	git(dir, "config", "user.email", "fixture@example.invalid")
	git(dir, "commit", "--quiet", "--allow-empty", "-m", "reviewed")
	reviewed := git(dir, "rev-parse", "HEAD")
	remote := t.TempDir()
	git(remote, "init", "--bare", "--quiet", "--initial-branch=trunk")
	git(dir, "remote", "add", "origin", remote)
	git(dir, "push", "--quiet", "origin", "trunk")
	git(dir, "switch", "--quiet", "-c", "unmerged")
	git(dir, "commit", "--quiet", "--allow-empty", "-m", "unreviewed")
	unmerged := git(dir, "rev-parse", "HEAD")

	guard := publisherStepByID(t, "default-branch")
	run := func(sha string) (string, error) {
		command := exec.Command("bash", "-c", guard.Run)
		command.Dir = dir
		command.Env = append(os.Environ(), "GH_TOKEN=fixture-only", "GITHUB_SHA="+sha, "GITHUB_REF_NAME=trunk")
		out, err := command.CombinedOutput()
		return string(out), err
	}
	out, err := run(reviewed)
	require.NoError(t, err, "%s", out)
	out, err = run(unmerged)
	require.Error(t, err, "%s", out)
	require.Contains(t, out, "not reachable from refs/remotes/origin/trunk")
	git(remote, "symbolic-ref", "HEAD", "refs/heads/missing")
	out, err = run(reviewed)
	require.Error(t, err, "%s", out)
	require.Contains(t, out, "cannot read this repository's default branch")
	config, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	require.NoError(t, err)
	require.NotContains(t, string(config), "extraheader", "authentication must not persist in the checkout")
}

func TestPublisherValidatesVersionAndRecordsStrictLock(t *testing.T) {
	run := func(step publisherStep, env ...string) (string, error) {
		t.Helper()
		dir := t.TempDir()
		command := exec.Command("bash", "-c", step.Run)
		command.Dir = dir
		command.Env = append(os.Environ(), append(env,
			"GITHUB_OUTPUT="+filepath.Join(dir, "outputs"),
			"GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"))...)
		out, err := command.CombinedOutput()
		if err != nil {
			return dir, err
		}
		require.NotContains(t, string(out), "error")
		return dir, nil
	}
	inputs := publisherStepByID(t, "inputs")
	dir, err := run(inputs, "REPOSITORY=codefly-dev/service-warehouse", "VERSION=0.0.2")
	require.NoError(t, err)
	output, err := os.ReadFile(filepath.Join(dir, "outputs"))
	require.NoError(t, err)
	require.Contains(t, string(output), "image=ghcr.io/codefly-dev/service-warehouse\nversion=0.0.2\n")
	for _, version := range []string{"", "v0.0.2", "0.0.2-rc1", "0.0.2\nimage=other"} {
		_, err = run(inputs, "REPOSITORY=codefly-dev/service-warehouse", "VERSION="+version)
		require.Error(t, err, "version %q must be rejected", version)
	}
	lock := publisherStepByID(t, "lock")
	valid := "sha256:" + strings.Repeat("a", 64)
	dir, err = run(lock, "IMAGE=ghcr.io/codefly-dev/service-warehouse", "DIGEST="+valid)
	require.NoError(t, err)
	content, err := os.ReadFile(filepath.Join(dir, "release-evidence", "gateway-image.json"))
	require.NoError(t, err)
	image, err := resources.ParseImageLock(content, gatewayImageName)
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/codefly-dev/service-warehouse@"+valid, image.FullName())
	for _, digest := range []string{"", "sha256:" + strings.Repeat("A", 64), "sha256:abc"} {
		_, err = run(lock, "IMAGE=ghcr.io/codefly-dev/service-warehouse", "DIGEST="+digest)
		require.Error(t, err, "digest %q must be rejected", digest)
	}
}
