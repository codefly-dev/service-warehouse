// Package distribution records how service-warehouse is shipped and holds that
// record to the repository's contents. The service publishes no container
// image: it builds to a Go binary, and nothing here builds, pushes, or deploys
// an image.
//
// Under the fleet image-SBOM contract (codefly-dev/core docs/sbom.md, released
// in v0.3.29) that is the NO_IMAGE_REASON_NO_IMAGE case. It is a statement
// about this repository, not about an agent: no Builder is implemented in this
// tree, so no Builder.SBOM RPC is served and none is claimed. Image evidence
// therefore has to land with image distribution rather than after it, which is
// what ImageSignals gates.
package distribution

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// workflowDir is the only place whose file contents are scanned. A build
// command is a signal when CI runs it, not when source or documentation names
// it.
var workflowDir = filepath.Join(".github", "workflows")

// imageBuildCommands emit an image when a workflow runs them.
var imageBuildCommands = []string{
	"docker build",
	"docker buildx",
	"docker/build-push-action",
}

var composeFile = regexp.MustCompile(`^(docker-)?compose([.\w-]+)?\.ya?ml$`)

// ImageSignals walks root and returns each path showing that this repository
// builds, publishes, or deploys a container image, sorted and annotated with
// what makes it a signal. An empty result is the no-image status holding.
func ImageSignals(root string) ([]string, error) {
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
		if filepath.Dir(rel) != workflowDir {
			return nil
		}
		workflow, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, command := range imageBuildCommands {
			if bytes.Contains(workflow, []byte(command)) {
				signals = append(signals, rel+": workflow runs "+command)
				return nil
			}
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
	}
	return ""
}
