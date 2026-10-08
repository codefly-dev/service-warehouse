package imageevidence

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/services/sbom"
	"github.com/stretchr/testify/require"
)

const pinned = "ghcr.io/codefly-dev/service-warehouse@sha256:1205da0743e56b68c12c754438476e8fdd0f05cdb08ce1c9687ddb8d8f0e1669"

func TestSubjectsAreOnePerShippedPlatformAndPinned(t *testing.T) {
	subjects, err := Subjects(context.Background(), "codefly.dev/warehouse", pinned, false)
	require.NoError(t, err)

	var platforms []string
	for _, subject := range subjects {
		platforms = append(platforms, subject.GetPlatform())
		require.Equal(t, pinned, subject.GetReference())
		require.Equal(t, Role, subject.GetRole())
		require.Equal(t, "codefly.dev/warehouse", subject.GetService())
		require.Equal(t, sbom.SourceRegistry, sbom.SourceOf(subject))
		require.NoError(t, sbom.RequirePinned(subject))
	}
	require.ElementsMatch(t, Platforms, platforms)
}

// An evidence request that names no service would match evidence belonging to
// any service, so core refuses it; this package does not paper over that.
func TestSubjectsRefuseAnUnnamedServiceAndAFloatingReference(t *testing.T) {
	_, err := Subjects(context.Background(), "", pinned, false)
	require.Error(t, err)
	_, err = Subjects(context.Background(), "codefly.dev/warehouse", "ghcr.io/codefly-dev/service-warehouse:latest", false)
	require.Error(t, err)
}

// A local subject carries the image ID the daemon holds, so an image the daemon
// does not hold can only be reported, never turned into a subject pinned to
// nothing. The resolved case needs a real image and is asserted by the e2e
// inventory.
func TestALocalSubjectNeedsAnImageTheDaemonHolds(t *testing.T) {
	const absent = "swh-imageevidence-unit-test-absent:none"
	subjects, err := Subjects(context.Background(), "codefly.dev/warehouse", absent, true)
	require.Error(t, err, "got %d subjects", len(subjects))
	require.Contains(t, err.Error(), absent)
}

func TestCollectRefusesToReportCoverageOfNothing(t *testing.T) {
	_, err := Collect(context.Background(), t.TempDir(), nil)
	require.Error(t, err)
}

func TestDocumentsAreNamedAfterTheImageThePlatformAndTheDigest(t *testing.T) {
	require.Equal(t, "service-warehouse-linux-arm64-1205da.cdx.json",
		documentName(pinned, "linux/arm64", "sha256:1205da"))
	require.Equal(t, "service-warehouse-linux-amd64-abc.cdx.json",
		documentName("service-warehouse:e2e", "linux/amd64", "sha256:abc"))
}

func TestReferenceNameDropsTheTagAndTheDigest(t *testing.T) {
	for reference, want := range map[string]string{
		pinned: "ghcr.io/codefly-dev/service-warehouse",
		"ghcr.io/codefly-dev/service-warehouse:0.0.1": "ghcr.io/codefly-dev/service-warehouse",
		"localhost:5000/service-warehouse:e2e":        "localhost:5000/service-warehouse",
		"service-warehouse":                           "service-warehouse",
		"service-warehouse:e2e":                       "service-warehouse",
	} {
		require.Equal(t, want, referenceName(reference), reference)
	}
}

func TestTheIndexNamesEveryDocumentWithItsPlatformAndDigest(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteIndex(dir, []Document{{
		Reference: pinned, Digest: "sha256:aa", Platform: "linux/amd64",
		Path: filepath.Join(dir, "a.cdx.json"), SHA256: "bb",
	}}))
	index, err := os.ReadFile(filepath.Join(dir, "index.txt"))
	require.NoError(t, err)
	require.Equal(t, "linux/amd64 ghcr.io/codefly-dev/service-warehouse@sha256:aa a.cdx.json bb\n", string(index))
}
