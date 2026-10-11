package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type publisherStep struct {
	ID   string            `yaml:"id"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
	With map[string]any    `yaml:"with"`
}

type publisherJob struct {
	Uses        string            `yaml:"uses"`
	If          string            `yaml:"if"`
	Timeout     int               `yaml:"timeout-minutes"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []publisherStep   `yaml:"steps"`
}

// corePublisher is the shared composite action that does the publishing, pinned
// by full commit SHA.
const corePublisher = "codefly-dev/core/.github/actions/publish-service-image@"

func loadPublisher(t *testing.T) publisherJob {
	t.Helper()
	var workflow struct {
		Jobs map[string]publisherJob `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(workflowBody(t, "publish-gateway-image.yml")), &workflow))
	require.Len(t, workflow.Jobs, 1)
	return workflow.Jobs["publish"]
}

// publisherAction is the step calling core's composite action, with its index so
// a test can assert what may run before it.
func publisherAction(t *testing.T) (publisherStep, int) {
	t.Helper()
	for i, step := range loadPublisher(t).Steps {
		if strings.HasPrefix(step.Uses, corePublisher) {
			return step, i
		}
	}
	t.Fatalf("the publisher does not call %s", corePublisher)
	return publisherStep{}, -1
}

// GHCR checks the defining repo. A job-level reusable workflow call broke the
// live object-storage release even with a caller-supplied registry token, which
// is why core ships the publisher as a composite action and this job — its event
// gate, permissions, timeout and checkout — stays here.
func TestPublisherKeepsRegistryAuthorityInServiceRepository(t *testing.T) {
	job := loadPublisher(t)
	require.Empty(t, job.Uses, "the publisher job must be defined in this repository")
	require.Equal(t, "github.event_name == 'workflow_dispatch'", job.If)
	require.Equal(t, 30, job.Timeout, "the action cannot declare job policy; the caller owns the timeout")
	require.Equal(t, "write", job.Permissions["packages"])
	require.Equal(t, "read", job.Permissions["contents"])

	action, at := publisherAction(t)
	require.Equal(t, "${{ secrets.GITHUB_TOKEN }}", action.With["registry-token"],
		"the registry credential must be this job's own token")
	require.Equal(t, "${{ inputs.version }}", action.With["version"])

	checkout := -1
	for i, step := range job.Steps {
		if strings.HasPrefix(step.Uses, "actions/checkout@") {
			checkout = i
			require.Equal(t, false, step.With["persist-credentials"],
				"a persisted credential enters the build context and the action refuses it")
			require.Equal(t, 0, step.With["fetch-depth"],
				"the action proves the commit is on the default branch, which needs full history")
			require.NotContains(t, step.With, "ref",
				"the action publishes the triggering commit; a ref override selects another tree")
		}
	}
	require.GreaterOrEqual(t, checkout, 0, "the publisher never checks out the commit it publishes")
	require.Less(t, checkout, at)
}

// The action's guard refuses a dirty tree, stored Git credentials and an
// unmerged commit, and it runs before registry login and before the build. A
// step inserted ahead of it that writes into the checkout, or runs this
// repository's code, is work the guard was meant to cover.
func TestPublisherRunsNothingOfItsOwnBeforeTheGuard(t *testing.T) {
	job := loadPublisher(t)
	_, at := publisherAction(t)
	for _, step := range job.Steps[:at] {
		require.Empty(t, step.Run, "a shell step runs before the action's guard")
		require.Contains(t, []string{"actions/checkout", "docker/setup-qemu-action"},
			strings.SplitN(step.Uses, "@", 2)[0],
			"only the checkout and the cgo cross-build emulator may precede the guard")
	}
}

// Legacy means deleted, not disabled: the local build, login, lock and upload
// steps are core's now, and a fallback left beside the action would publish
// without its guard.
func TestPublisherKeepsNoLocalPublishingSteps(t *testing.T) {
	job := loadPublisher(t)
	for _, step := range job.Steps {
		for _, gone := range []string{
			"docker/login-action",
			"docker/build-push-action",
			"docker/setup-buildx-action",
			"actions/upload-artifact",
		} {
			require.NotContains(t, step.Uses, gone, "%s is the action's work now", gone)
		}
		require.NotContains(t, step.Run, "imagetools")
		require.NotContains(t, step.Run, "jq -n")
	}
	body := workflowBody(t, "publish-gateway-image.yml")
	require.NotContains(t, body, "STOPGAP", "the stopgap it described is gone")
	require.Equal(t, 1, strings.Count(body, "secrets.GITHUB_TOKEN"),
		"the job's token must reach exactly one place, the action's registry-token input")
}

// The action writes the lock under the name this input gives it, and the agent
// embeds that file by name. Left to the action's default the two agree today
// and would diverge silently the day core changes it.
func TestPublisherRecordsTheLockTheAgentEmbeds(t *testing.T) {
	action, _ := publisherAction(t)
	require.Equal(t, "gateway-image.json", action.With["lock-file"])

	_, err := os.Stat("gateway-image.json")
	require.NoError(t, err, "the publisher records a lock file this repository does not keep")
	require.Contains(t, workflowBody(t, "release.yml"), "gateway-image.json",
		"the release reads a lock the publisher does not write")
}

// The image name is not an input here: the action derives it from the calling
// repository inside core's registry, which is what keeps the published image and
// the one the agent runs the same (TestGatewayImageNameIsThisRepository).
func TestPublisherDoesNotRenameTheImage(t *testing.T) {
	action, _ := publisherAction(t)
	require.NotContains(t, action.With, "image-name",
		"an override decouples the published image from this repository")
}

// Every core integration is pinned to one immutable commit, never a tag or a
// branch: release.yml passes `secrets: inherit`, so a mutable ref would hand an
// upstream change this repository's release PAT. The comment beside each pin
// names the core release, and that release is the one go.mod requires — the
// "bump the two together" rule the files state, executed.
func TestCorePinsAreOneImmutableCommitMatchingGoMod(t *testing.T) {
	gomod, err := os.ReadFile("go.mod")
	require.NoError(t, err)
	required := regexp.MustCompile(`github\.com/codefly-dev/core (v\S+)`).FindSubmatch(gomod)
	require.NotNil(t, required, "go.mod requires no codefly-dev/core")
	version := string(required[1])

	pin := regexp.MustCompile(`codefly-dev/core/\.github/\S+@([0-9a-f]{40}) # (v\S+)`)
	shas := map[string][]string{}
	for _, workflow := range []string{"publish-gateway-image.yml", "agent-ci.yml", "release.yml"} {
		body := workflowBody(t, workflow)
		found := pin.FindAllStringSubmatch(body, -1)
		require.NotEmpty(t, found, "%s calls core without a 40-hex SHA pin carrying its # vX.Y.Z", workflow)
		for _, match := range found {
			require.Equal(t, version, match[2],
				"%s pins core %s while go.mod requires %s; bump them together", workflow, match[2], version)
			shas[match[1]] = append(shas[match[1]], workflow)
		}
		require.NotRegexp(t, `codefly-dev/core/\.github/\S+@(main|v[0-9])`, body,
			"%s pins core by a mutable ref", workflow)
	}
	require.Len(t, shas, 1, "the core integrations are pinned to different commits: %v", shas)
}
