package distribution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is this repository, two directories above internal/distribution.
const repoRoot = "../.."

// TestRepositoryShipsNoImage is the gate behind the documented no-image status.
// The moment this repository builds, publishes, or deploys an image, the status
// is false and image-scope SBOM evidence has to land with the image.
func TestRepositoryShipsNoImage(t *testing.T) {
	found, err := ImageSignals(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range found {
		t.Errorf("image-producing path found: %s", signal)
	}
	if len(found) > 0 {
		t.Log("service-warehouse documents no image, so nothing here publishes image SBOM evidence. " +
			"Distributing an image requires a CycloneDX SBOM per final image — OS packages and " +
			"application dependencies — bound to the digest and platform actually shipped, served " +
			"through the fleet contract (codefly-dev/core docs/sbom.md: BuilderWrapper.SBOMImages, " +
			"checked by sbom.ValidateCoverage), then README.md and this gate updated to match.")
	}
}

func TestImageSignalsFindsAnImageBuildDefinition(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Dockerfile", "FROM scratch\n")

	requireSignal(t, root, "Dockerfile")
}

func TestImageSignalsFindsAnAgentManifest(t *testing.T) {
	root := t.TempDir()
	write(t, root, "agent.codefly.yaml", "kind: service\n")

	requireSignal(t, root, "agent.codefly.yaml")
}

func TestImageSignalsFindsACompose(t *testing.T) {
	root := t.TempDir()
	write(t, root, "docker-compose.yaml", "services:\n  warehouse:\n    image: warehouse:latest\n")

	requireSignal(t, root, "docker-compose.yaml")
}

func TestImageSignalsFindsAWorkflowImageBuild(t *testing.T) {
	for _, command := range imageBuildCommands {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, filepath.Join(".github", "workflows", "release.yml"),
				"jobs:\n  publish:\n    steps:\n      - run: "+command+" -t warehouse .\n")

			requireSignal(t, root, filepath.Join(".github", "workflows", "release.yml"))
		})
	}
}

// TestImageSignalsIgnoresNamingWithoutAnImage keeps the gate honest in the
// other direction: it scans workflow contents only, so source and configuration
// that merely name a build command are not a signal — otherwise this package
// and its own documentation would trip it.
func TestImageSignalsIgnoresNamingWithoutAnImage(t *testing.T) {
	root := t.TempDir()
	write(t, root, "README.md", "Introducing an image means running `docker buildx build`.\n")
	write(t, root, "build_test.go", "// docker/build-push-action is not run here\n")
	write(t, root, "buf.yaml", "version: v2\n")
	write(t, root, filepath.Join(".github", "workflows", "ci.yml"), "jobs:\n  test:\n    steps:\n      - run: go test ./...\n")

	found, err := ImageSignals(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("no image is produced, got signals %v", found)
	}
}

func requireSignal(t *testing.T, root, want string) {
	t.Helper()
	found, err := ImageSignals(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("want one signal for %s, got %v", want, found)
	}
	if got, _, _ := strings.Cut(found[0], ": "); got != want {
		t.Errorf("signal names %s, want %s", got, want)
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
