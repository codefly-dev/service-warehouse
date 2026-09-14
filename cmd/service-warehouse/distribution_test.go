package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot is this repository, two directories above cmd/service-warehouse.
const repoRoot = "../.."

// workflowDir holds the files whose contents are scanned for build commands. A
// build command is a signal when CI runs it, not when source or documentation
// names it.
var workflowDir = filepath.Join(".github", "workflows")

// imageBuildCommands emit an image when a workflow runs them.
var imageBuildCommands = []string{
	"docker build",
	"docker buildx",
	"docker/build-push-action",
}

// manifestExtensions can name an image this service deploys. Evidence binds to
// the digest actually built *or selected for deployment*, so an image this
// repository deploys without building it still needs coverage, and a manifest
// is scanned wherever in the tree it lives.
var manifestExtensions = map[string]bool{".yaml": true, ".yml": true, ".tmpl": true, ".tpl": true}

// imageRefKey matches the keys carrying a container image in a Kubernetes
// manifest, a Helm value file, or a Kustomize image transformer. The value is
// deliberately unconstrained: the fleet's manifests template it
// (`image: {{ .Image }}`), and a templated reference still means an image is
// deployed.
var imageRefKey = regexp.MustCompile(`(?m)^\s*-?\s*(images?|newName):`)

var composeFile = regexp.MustCompile(`^(docker-)?compose([.\w-]+)?\.ya?ml$`)

// imageSignals walks root and returns each path showing that this repository
// builds, publishes, or deploys a container image, sorted and annotated with
// what makes it a signal. An empty result is the no-image status holding.
func imageSignals(root string) ([]string, error) {
	var signals []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if reason := nameSignal(entry.Name()); reason != "" {
			signals = append(signals, rel+": "+reason)
			return nil
		}
		reason, err := contentSignal(path, rel)
		if err != nil {
			return err
		}
		if reason != "" {
			signals = append(signals, rel+": "+reason)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(signals)
	return signals, nil
}

func nameSignal(name string) string {
	switch {
	case strings.HasPrefix(name, "Dockerfile"), strings.HasPrefix(name, "Containerfile"):
		return "container image build definition"
	case name == "agent.codefly.yaml":
		return "Codefly agent manifest, whose Builder ships image build recipes"
	case composeFile.MatchString(name):
		return "compose file selecting runtime images"
	case name == "kustomization.yaml", name == "kustomization.yml", name == "Chart.yaml":
		return "deployment overlay that resolves runtime images"
	}
	return ""
}

// contentSignal reports what a file's contents reveal: a build command when CI
// runs one, or an image reference when a deployment manifest names one. A
// workflow is never read as a manifest, so a CI service container is not
// mistaken for a shipped image.
func contentSignal(path, rel string) (string, error) {
	inWorkflow := filepath.Dir(rel) == workflowDir
	if !inWorkflow && !manifestExtensions[filepath.Ext(rel)] {
		return "", nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if inWorkflow {
		for _, command := range imageBuildCommands {
			if bytes.Contains(content, []byte(command)) {
				return "workflow runs " + command, nil
			}
		}
		return "", nil
	}
	if imageRefKey.Match(content) {
		return "manifest names a container image to deploy", nil
	}
	return "", nil
}

// TestRepositoryShipsNoImage is the gate behind the documented no-image status.
// The moment this repository builds, publishes, or deploys an image, the status
// is false and image-scope SBOM evidence has to land with the image.
func TestRepositoryShipsNoImage(t *testing.T) {
	found, err := imageSignals(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range found {
		t.Errorf("image-producing path found: %s", signal)
	}
	if len(found) > 0 {
		t.Log("service-warehouse documents no image, so nothing here publishes image SBOM evidence. " +
			"Shipping or deploying an image requires a CycloneDX SBOM per final image — OS packages " +
			"and application dependencies — bound to the digest and platform actually shipped, served " +
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
	// Spelled out rather than ranged over imageBuildCommands: a test driven by
	// the production list cannot fail when a command is dropped from it, which
	// would silently narrow the gate.
	commands := []string{"docker build", "docker buildx", "docker/build-push-action"}
	if len(commands) != len(imageBuildCommands) {
		t.Fatalf("imageBuildCommands is %v; update this test to match", imageBuildCommands)
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, filepath.Join(".github", "workflows", "release.yml"),
				"jobs:\n  publish:\n    steps:\n      - run: "+command+" -t warehouse .\n")

			requireSignal(t, root, filepath.Join(".github", "workflows", "release.yml"))
		})
	}
}

// TestImageSignalsFindsADeployedImage covers an image this repository deploys
// without building: the evidence contract binds to the digest built *or
// selected for deployment*, so a manifest naming an image is image coverage
// this repository would owe.
func TestImageSignalsFindsADeployedImage(t *testing.T) {
	manifests := map[string]string{
		"digest-pinned deployment":  "spec:\n  containers:\n    - name: warehouse\n      image: ghcr.io/codefly-dev/service-warehouse@sha256:abc\n",
		"templated deployment":      "spec:\n  containers:\n    - image: {{ .Image }}\n",
		"helm values":               "image:\n  repository: ghcr.io/codefly-dev/service-warehouse\n  tag: v1\n",
		"kustomize image override":  "images:\n  - name: warehouse\n    newName: ghcr.io/codefly-dev/service-warehouse\n",
		"kustomize template suffix": "spec:\n  containers:\n    - image: {{ .Image }}\n",
	}
	names := map[string]string{
		"digest-pinned deployment":  filepath.Join("deploy", "deployment.yaml"),
		"templated deployment":      filepath.Join("templates", "deployment", "deployment.yaml.tmpl"),
		"helm values":               filepath.Join("charts", "warehouse", "values.yaml"),
		"kustomize image override":  filepath.Join("deploy", "overlays", "images.yaml"),
		"kustomize template suffix": filepath.Join("templates", "svc.tpl"),
	}
	for name, content := range manifests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, names[name], content)

			requireSignal(t, root, names[name])
		})
	}
}

func TestImageSignalsFindsADeploymentOverlay(t *testing.T) {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Chart.yaml"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, filepath.Join("deploy", name), "resources:\n  - deployment.yaml\n")

			requireSignal(t, root, filepath.Join("deploy", name))
		})
	}
}

// TestImageSignalsIgnoresNamingWithoutAnImage keeps the gate honest in the
// other direction. Contents are read only for workflows and manifests, so
// source and documentation that merely name a build command are not a signal —
// otherwise this file and the README would trip it — and a workflow's own
// service container is CI infrastructure rather than a shipped image.
func TestImageSignalsIgnoresNamingWithoutAnImage(t *testing.T) {
	root := t.TempDir()
	write(t, root, "README.md", "Introducing an image means running `docker buildx build`.\n")
	write(t, root, "build_test.go", "// docker/build-push-action is not run here\n")
	write(t, root, "buf.yaml", "version: v2\n")
	write(t, root, "config.yaml", "listen: :9465\nimage_name_prefix: unrelated\n")
	write(t, root, filepath.Join(".github", "workflows", "ci.yml"),
		"jobs:\n  test:\n    container:\n      image: golang:1.27\n    steps:\n      - run: go test ./...\n")

	found, err := imageSignals(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("no image is produced, got signals %v", found)
	}
}

func requireSignal(t *testing.T, root, want string) {
	t.Helper()
	found, err := imageSignals(root)
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
