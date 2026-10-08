//go:build e2e

// This file drives the agent's image-SBOM path against a real gateway image,
// inventorying it exactly as a release does and checking the result against the
// shared coverage contract. Run with:
//
//	SWH_GATEWAY_IMAGE=<locally-built gateway image> go test -tags e2e -run TestImageSBOM .
//
// It needs syft on PATH: an image the local daemon holds and no registry serves
// is only reachable by a scanner with Docker access, which the managed
// containerized scanner deliberately does not have.
//
// Writing the evidence out is the release command's job (cmd/image-sbom), not
// this test's.
package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/services/sbom"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

func TestImageSBOMInventoriesTheGatewayImage(t *testing.T) {
	override := os.Getenv(gatewayImageOverrideEnv)
	if override == "" {
		t.Skipf("set %s to a locally built gateway image", gatewayImageOverrideEnv)
	}
	if _, err := exec.LookPath("syft"); err != nil {
		t.Skip("syft is required to inventory an image held only by the local daemon")
	}
	builder := newSBOMBuilder(t)
	subjects, err := builder.imageSubjects(context.Background())
	require.NoError(t, err)

	// The daemon resolves the override to its own image ID, so that ID is what a
	// scan of it binds evidence to and what the subject has to name.
	require.Len(t, subjects, 1, "the single platform the daemon holds")
	require.Equal(t, sbom.SourceDockerDaemon, sbom.SourceOf(subjects[0]))
	require.Equal(t, override, subjects[0].GetReference())
	require.Empty(t, subjects[0].GetPlatform(), "the platform is read from the image rather than assumed")
	require.NoError(t, sbom.RequirePinned(subjects[0]))

	resp, err := builder.SBOM(context.Background(), &builderv0.SBOMRequest{Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE})
	require.NoError(t, err)
	require.Equal(t, builderv0.SBOMStatus_COMPLETE, resp.GetState().GetState(), resp.GetState().GetMessage())
	require.NoError(t, sbom.ValidateCoverage(testService, subjects, resp))

	require.Len(t, resp.GetImages(), 1)
	evidence := resp.GetImages()[0]

	// The subject named the local image ID before the scan ran; evidence that
	// resolved to any other identity describes an image nobody asked for.
	require.Equal(t, subjects[0].GetDigest(), evidence.GetDigest())
	require.True(t, strings.HasPrefix(evidence.GetDigest(), "sha256:"))
	require.NotEmpty(t, evidence.GetPlatform(), "evidence names no platform")
	require.NotEmpty(t, evidence.GetSha256(), "evidence carries no document checksum")

	var goModules, osPackages int
	for _, component := range evidence.GetBom().GetComponents() {
		switch purl := component.GetPurl(); {
		case strings.HasPrefix(purl, "pkg:golang/"):
			goModules++
		case isOSPackage(purl):
			osPackages++
		}
	}
	require.NotZero(t, goModules, "inventory found no Go modules: the gateway's application dependencies are missing")
	require.NotZero(t, osPackages, "inventory found no OS packages: an image inventory is not satisfied by application dependencies alone")
	t.Logf("inventoried %s on %s (%d Go modules, %d OS packages)", evidence.GetDigest(), evidence.GetPlatform(), goModules, osPackages)
}

// osPackageTypes are the package-URL namespaces an OS inventory reports under.
// Most of what syft finds in an image carries no package URL at all, so counting
// everything that is not a Go module would let an image with no OS inventory
// pass as covered.
var osPackageTypes = []string{"pkg:deb/", "pkg:apk/", "pkg:rpm/"}

func isOSPackage(purl string) bool {
	for _, prefix := range osPackageTypes {
		if strings.HasPrefix(purl, prefix) {
			return true
		}
	}
	return false
}
