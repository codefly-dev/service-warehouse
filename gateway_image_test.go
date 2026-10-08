package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/service-warehouse/internal/auth"
	"github.com/codefly-dev/service-warehouse/internal/config"

	// The mem backend is registered here, in a test, so config.FromEnv can
	// resolve a configuration without a warehouse. The agent itself imports no
	// backend: see TestAgentLinksNoBackendDriver.
	_ "github.com/codefly-dev/service-warehouse/internal/backend/mem"
)

const (
	registry = "ghcr.io/codefly-dev"
	digest   = "sha256:1205da0743e56b68c12c754438476e8fdd0f05cdb08ce1c9687ddb8d8f0e1669"
)

func TestParseGatewayImageLock(t *testing.T) {
	image, err := parseGatewayImageLock([]byte(`{
		"name": "` + registry + `/service-warehouse",
		"digest": "` + digest + `"
	}`))
	require.NoError(t, err)
	require.Equal(t, registry+"/service-warehouse@"+digest, image.FullName())
}

// A lock the agent accepts but that names no immutable image is the failure
// pinning exists to prevent: it produces subjects whose evidence describes
// whatever the registry served, so every malformed shape is refused at load
// rather than carried into a scan.
func TestParseGatewayImageLockRejectsAnythingButASHA256Digest(t *testing.T) {
	name := registry + "/service-warehouse"
	for label, lock := range map[string]string{
		"no name":          `{"digest": "` + digest + `"}`,
		"another image":    `{"name": "` + registry + `/service-object-storage", "digest": "` + digest + `"}`,
		"another registry": `{"name": "registry.example.com/codefly-dev/service-warehouse", "digest": "` + digest + `"}`,
		"tag as digest":    `{"name": "` + name + `", "digest": "0.0.3"}`,
		"other algorithm":  `{"name": "` + name + `", "digest": "sha512:1205da07"}`,
		"truncated":        `{"name": "` + name + `", "digest": "sha256:1205da07"}`,
		"not hex":          `{"name": "` + name + `", "digest": "sha256:zzzzda0743e56b68c12c754438476e8fdd0f05cdb08ce1c9687ddb8d8f0e1669"}`,
		"not json":         `sha256:1205da07`,
	} {
		t.Run(label, func(t *testing.T) {
			_, err := parseGatewayImageLock([]byte(lock))
			require.Error(t, err, "accepted a lock that pins no image")
			require.NotErrorIs(t, err, errGatewayImageUnpublished)
		})
	}
}

// A lock with no digest is the tree before its first image was published. A
// reference with no digest and no tag is the registry's :latest, so the only
// safe reading is a refusal that says what to do.
func TestAnUnpublishedLockIsRefusedNeverRunAsLatest(t *testing.T) {
	image, err := parseGatewayImageLock([]byte(`{"name": "` + registry + `/service-warehouse", "digest": ""}`))
	require.ErrorIs(t, err, errGatewayImageUnpublished)
	require.Nil(t, image)
	require.Contains(t, err.Error(), "publish-gateway-image.yml")
}

// The embedded copy is what the agent runs; the file on disk is what the
// release reads to tag and to inventory. They are the same bytes, and a test
// reading the file keeps the embed from silently going stale.
func TestGatewayImageMatchesTheLockOnDisk(t *testing.T) {
	lock, err := os.ReadFile("gateway-image.json")
	require.NoError(t, err)
	require.Equal(t, string(lock), string(gatewayImageLockJSON))

	want, wantErr := parseGatewayImageLock(lock)
	got, gotErr := publishedGatewayImage()
	require.Equal(t, wantErr, gotErr)
	if wantErr == nil {
		require.Equal(t, want.FullName(), got.FullName())
	}
}

// The override names a locally built image for a run. It must never decide what
// a deployment or an audit describes, which is the image a release ships.
func TestTheOverrideDecidesTheRunButNotThePublishedImage(t *testing.T) {
	t.Setenv(gatewayImageOverrideEnv, "service-warehouse:local")

	image, overridden, err := effectiveGatewayImage()
	require.NoError(t, err)
	require.True(t, overridden)
	require.Equal(t, "service-warehouse:local", image.FullName())

	published, err := publishedGatewayImage()
	if err == nil {
		require.NotEqual(t, "service-warehouse:local", published.FullName())
	} else {
		require.ErrorIs(t, err, errGatewayImageUnpublished)
	}

	t.Setenv(gatewayImageOverrideEnv, "")
	_, overridden, _ = effectiveGatewayImage()
	require.False(t, overridden)
}

// The image's name is derived, not typed: the agent takes the registry from core
// and the repository name from gatewayImageName, while the workflows publish to
// the repository they run in. This is where those meet, so that renaming the
// repository, or the constant, fails here instead of publishing one image and
// running another.
func TestGatewayImageNameIsThisRepository(t *testing.T) {
	gomod, err := os.ReadFile("go.mod")
	require.NoError(t, err)
	module := regexp.MustCompile(`(?m)^module github\.com/(\S+)$`).FindSubmatch(gomod)
	require.NotNil(t, module, "go.mod names no github.com module")
	repository := string(module[1])

	image := resources.PublishedImage(gatewayImageName, "")
	require.Equal(t, "ghcr.io/"+repository, image.Repository+"/"+image.Name,
		"the agent runs an image other than the one this repository publishes")

	lock, err := os.ReadFile("gateway-image.json")
	require.NoError(t, err)
	require.Contains(t, string(lock), `"name": "ghcr.io/`+repository+`"`)

	for _, workflow := range []string{"publish-gateway-image.yml", "release.yml"} {
		require.Contains(t, workflowBody(t, workflow), "IMAGE: ghcr.io/${{ github.repository }}", workflow)
	}
}

// The container port is stated once in the agent and handed to the gateway as
// SWH_LISTEN and to the templates as the container port. The gateway's own
// default and the image's EXPOSE are the other two statements of it, and none of
// the three is derived from another.
func TestGatewayContainerPortIsTheGatewaysDefault(t *testing.T) {
	t.Setenv("SWH_BACKEND", "mem")
	t.Setenv("SWH_ALLOW_ANONYMOUS", "true")
	t.Setenv("SWH_AUTH_TOKEN", "")
	t.Setenv("SWH_LISTEN", "")

	cfg, err := config.FromEnv()
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf(":%d", gatewayContainerPort), cfg.ListenAddr)

	dockerfile, err := os.ReadFile("Dockerfile")
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(fmt.Sprintf(`(?m)^EXPOSE %d$`, gatewayContainerPort)), string(dockerfile))
}

// The agent presents the token under the key the gateway enforces. internal/auth
// redeclares the key because the gateway must not link core's agent runtime; the
// agent does link it, so this is where a divergence is caught.
func TestTheTokenKeyIsTheOneCoreAuthenticatesAgentsWith(t *testing.T) {
	require.Equal(t, agents.AuthMetadataKey, auth.MetadataKey)
}

// The agent is released without cgo (.goreleaser.yaml) and must stay free of the
// gateway's backends: the embedded DuckDB engine is a cgo driver, and linking it
// here would turn the release's CGO_ENABLED=0 build into one that fails the day
// the driver lands.
func TestAgentLinksNoBackendDriver(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	require.NoError(t, err)
	for _, dependency := range strings.Fields(string(out)) {
		require.False(t, strings.Contains(dependency, "service-warehouse/internal/backend") ||
			strings.Contains(dependency, "duckdb"),
			"the agent links %s, a backend the gateway carries", dependency)
	}
}
