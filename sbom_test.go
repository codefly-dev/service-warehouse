package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services/sbom"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/codefly-dev/service-warehouse/internal/imageevidence"
)

// testService is the identity newSBOMBuilder loads, and therefore the identity
// every subject it produces carries.
const testService = "module/warehouse"

// pinnedReference is a digest-pinned image reference of the shape a release
// scans. It stands in for the published image so these tests mean the same thing
// before the first image is published and after.
const pinnedReference = registry + "/service-warehouse@" + digest

// newSBOMBuilder returns a Builder wired for a headless SBOM call.
func newSBOMBuilder(t *testing.T) *Builder {
	t.Helper()
	builder := NewBuilder()
	identity := &basev0.ServiceIdentity{
		Workspace: "workspace", Module: "module", Name: "warehouse", Version: "1.2.3",
		WorkspacePath: t.TempDir(), RelativeToWorkspace: ".",
	}
	require.NoError(t, builder.Base.HeadlessLoad(context.Background(), identity))
	return builder
}

func TestImageSubjectsCoverEveryPublishedPlatform(t *testing.T) {
	subjects, err := imageevidence.Subjects(context.Background(), testService, pinnedReference, false)
	require.NoError(t, err)

	require.Len(t, subjects, len(imageevidence.Platforms), "one subject per shipped platform")
	covered := map[string]bool{}
	for _, subject := range subjects {
		require.Equal(t, sbom.SourceRegistry, sbom.SourceOf(subject))
		require.NoError(t, sbom.RequirePinned(subject))
		require.Equal(t, pinnedReference, subject.GetReference())
		require.Equal(t, imageevidence.Role, subject.GetRole())
		require.Equal(t, testService, subject.GetService())
		covered[subject.GetPlatform()] = true
	}
	for _, platform := range imageevidence.Platforms {
		require.True(t, covered[platform], "no subject covers shipped platform %s", platform)
	}
}

// The registry derivation is core's, and it refuses what the evidence contract
// refuses: a tag serves whatever was pushed to it last, so a subject naming one
// says nothing about the image that was built.
func TestImageSubjectsRefuseAReferenceThatIsNotPinned(t *testing.T) {
	for _, reference := range []string{
		registry + "/service-warehouse",
		registry + "/service-warehouse:latest",
		registry + "/service-warehouse:0.0.1",
	} {
		_, err := imageevidence.Subjects(context.Background(), testService, reference, false)
		require.Error(t, err, reference)
	}
}

// What the Builder enumerates for itself is the pinned image gateway-image.json
// records, however many platforms that is. While the lock records no image there
// is nothing to enumerate, and saying so is the only honest answer.
func TestBuilderEnumeratesThePublishedImageOrRefuses(t *testing.T) {
	t.Setenv(gatewayImageOverrideEnv, "")
	builder := newSBOMBuilder(t)

	subjects, err := builder.imageSubjects(context.Background())
	if _, lockErr := publishedGatewayImage(); lockErr != nil {
		require.ErrorIs(t, err, lockErr)
		return
	}
	require.NoError(t, err)
	published, err := publishedGatewayImage()
	require.NoError(t, err)
	require.Len(t, subjects, len(imageevidence.Platforms))
	for _, subject := range subjects {
		require.Equal(t, published.FullName(), subject.GetReference())
		require.Equal(t, testService, subject.GetService())
	}
}

func TestImageEvidenceSatisfiesTheCoverageContract(t *testing.T) {
	subjects, err := imageevidence.Subjects(context.Background(), testService, pinnedReference, false)
	require.NoError(t, err)

	// The expectation is built from the platforms the release pipeline actually
	// publishes, never from imageevidence: deriving both sides from the code
	// under test compares it against itself, and would validate however many
	// platforms it dropped.
	expected := expectedFromReleasePipeline(t, pinnedReference)

	require.NoError(t, sbom.ValidateCoverage(testService, expected, imageResponse(evidenceFor(subjects))),
		"evidence for every published platform must validate as coverage")
	require.Error(t, sbom.ValidateCoverage(testService, expected, imageResponse(evidenceFor(subjects[:1]))),
		"evidence omitting a published platform must not validate as coverage")
}

// The daemon resolves whatever reference it is handed to its own image ID, so a
// subject for a local image has to carry that ID: evidence bound to an identity
// the subject never named is not coverage of it. Resolving it can fail — the
// override may name an image the daemon does not hold — and reporting that is
// the only alternative to producing a subject pinned to nothing.
//
// The resolved case needs a real image and is asserted by the e2e inventory.
func TestGatewayOverrideMustBeHeldByTheDaemon(t *testing.T) {
	const absent = "swh-gateway-unit-test-absent:none"
	t.Setenv(gatewayImageOverrideEnv, absent)

	subjects, err := newSBOMBuilder(t).imageSubjects(context.Background())
	require.Error(t, err, "got %d subjects for an image the daemon does not hold", len(subjects))
	require.Contains(t, err.Error(), absent, "the error must name the override that could not be resolved")
}

func TestImageScopeReportsFailureRatherThanCoverage(t *testing.T) {
	for name, setup := range map[string]func(*testing.T){
		"an override that is not an image": func(t *testing.T) {
			t.Setenv(gatewayImageOverrideEnv, "not a valid image reference")
		},
		"a lock that records no image": func(t *testing.T) {
			if _, err := publishedGatewayImage(); err == nil {
				t.Skip("gateway-image.json records a published image")
			}
			t.Setenv(gatewayImageOverrideEnv, "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			setup(t)
			resp, err := newSBOMBuilder(t).SBOM(context.Background(), &builderv0.SBOMRequest{Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE})
			require.NoError(t, err)

			require.Equal(t, builderv0.SBOMStatus_ERROR, resp.GetState().GetState())
			require.Equal(t, builderv0.SBOMScope_SBOM_SCOPE_IMAGE, resp.GetScope(), "the failure is attributed to the image request")
			require.Empty(t, resp.GetImages(), "a failed scan carries no inventory")
			require.Error(t, sbom.ValidateCoverage(testService, nil, resp), "a failed scan must not validate as coverage")
		})
	}
}

func TestSourceScopeKeepsTheExistingInventory(t *testing.T) {
	t.Setenv(gatewayImageOverrideEnv, "")
	subjects, err := imageevidence.Subjects(context.Background(), testService, pinnedReference, false)
	require.NoError(t, err)

	// A cancelled context stops the scan before it reaches the network, which is
	// enough to tell the two paths apart: the image path attributes even its
	// failures to SBOM_SCOPE_IMAGE, so an unspecified scope that does not come
	// back image-scoped was answered by the source inventory.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := newSBOMBuilder(t).SBOM(ctx, &builderv0.SBOMRequest{})
	require.NoError(t, err)

	require.NotEqual(t, builderv0.SBOMScope_SBOM_SCOPE_IMAGE, resp.GetScope(), "an unspecified scope must not be answered with image evidence")
	require.Error(t, sbom.ValidateCoverage(testService, subjects, resp), "a source inventory must not validate as image coverage")
}

// Serving the image scope is what lets the agent answer; advertising
// ValidationCapabilities.image_sbom is what gets the phase scheduled against
// every consumer, and core's docs/sbom.md holds that back until each of them
// runs a core that can represent it. A present Validation is also authoritative
// for every operation it omits, so setting one to advertise this capability
// would declare test unsupported for a service that has no suite to run. Flipping
// this is a rollout decision, which is why a test, not a comment, marks it.
func TestImageSBOMIsServedButNotAdvertised(t *testing.T) {
	info, err := NewService().GetAgentInformation(context.Background(), &agentv0.AgentInformationRequest{})
	require.NoError(t, err)
	require.Nil(t, info.GetValidation(), "advertising image_sbom is a fleet rollout decision, not this agent's")

	// And it is served: an explicit subject is scanned rather than answered as
	// unsupported, so the failure here is the scanner's, not a missing
	// implementation.
	subject := &builderv0.ImageSubject{Reference: "not-pinned", Service: testService, Role: imageevidence.Role}
	resp, err := newSBOMBuilder(t).SBOM(context.Background(), &builderv0.SBOMRequest{
		Scope:    builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
		Subjects: []*builderv0.ImageSubject{subject},
	})
	require.NoError(t, err)
	require.Equal(t, builderv0.SBOMStatus_ERROR, resp.GetState().GetState(), resp.GetState().GetMessage())
	require.NotEqual(t, builderv0.SBOMStatus_UNSUPPORTED, resp.GetState().GetState())
}

func TestShippedPlatformsMatchTheReleasePipeline(t *testing.T) {
	published := append([]string(nil), publishedPlatforms(t)...)
	declared := append([]string(nil), imageevidence.Platforms...)
	// Compare as sets: the pipeline and the code have to name the same
	// platforms, but neither owns the order the other writes them in.
	sort.Strings(published)
	sort.Strings(declared)

	require.Equal(t, published, declared,
		"the pipeline publishes %v but evidence covers %v; a platform shipped without a subject ships uninventoried", published, declared)
}

// The release once looped over a platform list written into the workflow and
// called the scanner itself, which is a second list to keep in sync and a
// second scanner to drift from the contract. Evidence comes from the release
// command, which reads the same platform list the agent does.
func TestReleaseEvidenceComesFromTheSharedCommand(t *testing.T) {
	body := workflowBody(t, "release.yml")

	require.Contains(t, body, "cmd/image-sbom", "the release workflow does not generate evidence through cmd/image-sbom")
	require.NotContains(t, body, "for platform in", "the release workflow loops over its own platform list")
	require.NotContains(t, workflowBody(t, "publish-gateway-image.yml"), "for platform in",
		"the publish workflow loops over its own platform list; the platforms it builds are read from its build-push step")
}

// expectedFromReleasePipeline builds the coverage expectation from the publish
// workflow, independently of the code that produces the evidence.
func expectedFromReleasePipeline(t *testing.T, reference string) []*builderv0.ImageSubject {
	t.Helper()
	platforms := publishedPlatforms(t)
	expected := make([]*builderv0.ImageSubject, 0, len(platforms))
	for _, platform := range platforms {
		expected = append(expected, &builderv0.ImageSubject{
			Reference: reference,
			Platform:  platform,
			Role:      imageevidence.Role,
			Service:   testService,
		})
	}
	return expected
}

// publishedPlatforms reads the platforms the pipeline actually builds, from the
// step that pushes the image rather than from any step that happens to carry a
// platform list.
func publishedPlatforms(t *testing.T) []string {
	t.Helper()
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string `yaml:"uses"`
				With struct {
					Platforms string `yaml:"platforms"`
				} `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(workflowBody(t, "publish-gateway-image.yml")), &workflow))
	var published []string
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if !strings.Contains(step.Uses, "docker/build-push-action") {
				continue
			}
			for _, platform := range strings.Split(step.With.Platforms, ",") {
				if platform = strings.TrimSpace(platform); platform != "" {
					published = append(published, platform)
				}
			}
		}
	}
	require.NotEmpty(t, published, "the publish workflow's build-push step publishes no platforms")
	return published
}

func workflowBody(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(".github", "workflows", name))
	require.NoError(t, err)
	return string(data)
}

// releaseStep is one step of the release workflow, kept in file order so a test
// can assert what happens before what.
type releaseStep struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
}

type releaseJob struct {
	Needs any           `yaml:"needs"`
	Uses  string        `yaml:"uses"`
	Steps []releaseStep `yaml:"steps"`
}

func releaseJobs(t *testing.T) map[string]releaseJob {
	t.Helper()
	var workflow struct {
		Jobs map[string]releaseJob `yaml:"jobs"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(workflowBody(t, "release.yml")), &workflow))
	return workflow.Jobs
}

// releaseStepRunning returns the one release step whose script contains marker.
func releaseStepRunning(t *testing.T, marker string) releaseStep {
	t.Helper()
	var found []releaseStep
	for _, job := range releaseJobs(t) {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, marker) {
				found = append(found, step)
			}
		}
	}
	require.Len(t, found, 1, "want exactly one release step running %q", marker)
	return found[0]
}

// stepIndex is where in the release job order marker first runs, counting the
// jobs in name order so a step added to another job shifts every index the same
// way on each run: Go randomises map iteration.
func stepIndex(t *testing.T, marker string) int {
	t.Helper()
	jobs := releaseJobs(t)
	names := make([]string, 0, len(jobs))
	for name := range jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	index := 0
	for _, name := range names {
		for _, step := range jobs[name].Steps {
			if strings.Contains(step.Run, marker) {
				return index
			}
			index++
		}
	}
	return -1
}

// The release used to push the tags consumers resolve and inventory the image
// afterwards. A failure anywhere in the evidence step then turned the run red
// with a pullable, uninventoried image already published — and a red run reads
// as "nothing shipped", so nobody goes looking for it. The image is now pushed
// by digest before the tag exists, and the tags are created only once evidence
// for that digest exists.
func TestReleasePublishesTagsOnlyAfterEvidence(t *testing.T) {
	for jobName, job := range releaseJobs(t) {
		for _, step := range job.Steps {
			require.NotContains(t, step.Uses, "docker/build-push-action",
				"%s builds the gateway image; the digest the agent pins cannot come from the tag that ships it, so the image is published before the tag", jobName)
		}
	}
	evidence, publish := stepIndex(t, "cmd/image-sbom"), stepIndex(t, "imagetools create")
	require.GreaterOrEqual(t, evidence, 0, "the release workflow does not generate image evidence")
	require.GreaterOrEqual(t, publish, 0, "the release workflow never creates the tags consumers resolve")
	require.Greater(t, publish, evidence, "release tags are created before evidence: a failed scan would leave a pullable image with no evidence")
}

// The gateway image the release tags is the one gateway-image.json records, and
// the agent runs that digest rather than a tag. Reading it from anywhere else —
// resolving :latest, or rebuilding — publishes tags for bytes the released
// agent never names.
func TestReleaseTagsTheRecordedDigest(t *testing.T) {
	publish := releaseStepRunning(t, "imagetools create")
	require.Contains(t, publish.Run, `"$IMAGE@$DIGEST"`, "the tag-publishing step does not create its tags from a recorded digest")

	locked := releaseStepRunning(t, "gateway-image.json")
	require.Contains(t, locked.Run, "org.opencontainers.image.version",
		"the release accepts the recorded digest without checking it was built for this version; a lock left unrefreshed publishes the previous release's image under the new tag")
}

// The release must refuse the tree as it is merged: gateway-image.json records
// no digest until the first image is published, and `jq -e` on its own passes an
// empty string. This runs the workflow's own guard against that file, not a
// description of it. It stops before any registry call, so it needs no network.
func TestReleaseRefusesALockThatRecordsNoImage(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required to run the release's lock check")
	}
	if _, err := publishedGatewayImage(); err == nil {
		t.Skip("gateway-image.json records a published image; this guards the state before one exists")
	}
	locked := releaseStepRunning(t, "gateway-image.json")
	lock, err := os.ReadFile("gateway-image.json")
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gateway-image.json"), lock, 0o600))
	command := exec.Command("bash", "-c", locked.Run)
	command.Dir = dir
	command.Env = append(os.Environ(), "VERSION=0.0.1", "IMAGE="+registry+"/service-warehouse")
	out, err := command.CombinedOutput()
	require.Error(t, err, "the release accepted a lock that records no image:\n%s", out)
	require.Contains(t, string(out), "no gateway image has been published")
}

// The published image tag is derived from the git tag while the agent reports
// the version of the embedded agent.codefly.yaml. Nothing reconciles them at run
// time, and a tag one release ahead of the file makes a consumer pinned to one
// version run another. This runs the workflow's own guard against the real file
// rather than trusting that it is present.
func TestReleaseGuardRejectsTagThatDisagreesWithAgentVersion(t *testing.T) {
	guard := releaseStepRunning(t, "agent.codefly.yaml")

	run := func(version string) error {
		command := exec.Command("bash", "-c", guard.Run)
		command.Env = append(os.Environ(), "VERSION="+version)
		return command.Run()
	}

	require.NoError(t, run(agent.Version), "guard rejected the version this repo actually carries")
	require.Error(t, run(agent.Version+"9"), "guard accepted a tag that disagrees with agent.codefly.yaml")
}

// needs is a job's `needs`, which GitHub accepts as one job name or a list.
func needs(job releaseJob) []string {
	switch value := job.Needs.(type) {
	case string:
		return []string{value}
	case []any:
		names := make([]string, 0, len(value))
		for _, name := range value {
			names = append(names, fmt.Sprint(name))
		}
		return names
	}
	return nil
}

// runsAfter reports whether job cannot start until earlier has succeeded.
func runsAfter(jobs map[string]releaseJob, job, earlier string) bool {
	seen := map[string]bool{}
	queue := needs(jobs[job])
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if next == earlier {
			return true
		}
		if !seen[next] {
			seen[next] = true
			queue = append(queue, needs(jobs[next])...)
		}
	}
	return false
}

// A `v*` tag must start one workflow, because separate workflows cannot order or
// gate each other: either could fail while the other published, and the harmful
// direction is the release becoming `latest` while no registry serves the image
// its agent names. Everything a tag publishes hangs off one chain, ordered by
// the dependency that actually exists.
func TestATagPublishesThroughOneOrderedWorkflow(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(".github", "workflows"))
	require.NoError(t, err)
	var onTag []string
	for _, entry := range entries {
		if !entry.Type().IsRegular() || (filepath.Ext(entry.Name()) != ".yml" && filepath.Ext(entry.Name()) != ".yaml") {
			continue
		}
		var workflow struct {
			On map[string]any `yaml:"on"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(workflowBody(t, entry.Name())), &workflow), entry.Name())
		push, ok := workflow.On["push"].(map[string]any)
		if !ok {
			continue
		}
		// A push trigger covers branches and tags alike: naming only branches
		// drops tags, while naming neither filter leaves both live.
		_, tags := push["tags"]
		_, tagsIgnore := push["tags-ignore"]
		_, branches := push["branches"]
		_, branchesIgnore := push["branches-ignore"]
		if tags || tagsIgnore || (!branches && !branchesIgnore) {
			onTag = append(onTag, entry.Name())
		}
	}
	require.Equal(t, []string{"release.yml"}, onTag, "a tag push must start one workflow")

	jobs := releaseJobs(t)
	var releases, images, gates []string
	for name, job := range jobs {
		if strings.Contains(job.Uses, "go-service-release.yml") {
			releases = append(releases, name)
		}
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "imagetools create") {
				images = append(images, name)
			}
			if strings.Contains(step.Run, "go test") {
				gates = append(gates, name)
			}
		}
	}
	require.Len(t, releases, 1, "a tag ships one release")
	require.NotEmpty(t, images)
	require.NotEmpty(t, gates)
	// Every image the tag publishes has to exist before the release names it,
	// and has to be gated on the suite. Quantifying over the sets keeps a job
	// added later from changing which pair happens to be checked.
	for _, image := range images {
		require.True(t, runsAfter(jobs, releases[0], image),
			"job %q does not wait for %q: the release can ship an agent whose gateway image no registry resolves", releases[0], image)
		require.True(t, slices.ContainsFunc(gates, func(gate string) bool { return runsAfter(jobs, image, gate) }),
			"job %q waits for none of %v: a tag whose tests fail still publishes an image tagged :latest", image, gates)
	}
}

// imageResponse wraps evidence in the complete image-scope response an agent
// returns.
func imageResponse(images []*builderv0.ImageSBOM) *builderv0.SBOMResponse {
	return &builderv0.SBOMResponse{
		State:  &builderv0.SBOMStatus{State: builderv0.SBOMStatus_COMPLETE},
		Scope:  builderv0.SBOMScope_SBOM_SCOPE_IMAGE,
		Images: images,
	}
}

// evidenceFor builds one digest-bound inventory per subject, each with a
// distinct digest so coverage is matched per platform.
func evidenceFor(subjects []*builderv0.ImageSubject) []*builderv0.ImageSBOM {
	images := make([]*builderv0.ImageSBOM, 0, len(subjects))
	for i, subject := range subjects {
		images = append(images, &builderv0.ImageSBOM{
			Digest:   fmt.Sprintf("sha256:%064d", i),
			Platform: subject.GetPlatform(),
			Subjects: []*builderv0.ImageSubject{subject},
			Bom:      &agentv0.Bom{Components: []*agentv0.Component{{Name: "libc", Version: "2.36"}}},
			Tool:     "syft",
			Sha256:   fmt.Sprintf("%064d", i),
		})
	}
	return images
}
