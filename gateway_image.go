package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// gatewayImageName is the repository, inside core's image registry, this
// service's gateway image is published to. The release workflows derive the same
// name from the repository they run in (ghcr.io/<owner>/<repo>), and
// TestGatewayImageNameIsThisRepository holds the two together.
const gatewayImageName = "service-warehouse"

// errGatewayImageUnpublished means gateway-image.json records no digest: the
// image has not been published for any version yet. It is a state the tree
// legitimately sits in between merging the agent and publishing its first
// image, and the only safe reading of it is a refusal. A reference with no
// digest and no tag is the registry's :latest, which is whatever was pushed
// last and which the evidence contract refuses to inventory.
var errGatewayImageUnpublished = errors.New("no gateway image has been published: gateway-image.json records no digest. " +
	"Run publish-gateway-image.yml for the upcoming version and commit the digest it prints")

// gatewayImageLock is the recorded identity of the published gateway image. It
// carries no tag: the tag is the agent version, and a second copy of it is one
// more thing to disagree with agent.codefly.yaml.
type gatewayImageLock struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// parseGatewayImageLock returns the image the lock pins. The registry comes from
// core, never from the lock: a lock naming another registry is the lock being
// wrong, not an instruction to run an image from there.
func parseGatewayImageLock(content []byte) (*resources.DockerImage, error) {
	var lock gatewayImageLock
	if err := json.Unmarshal(content, &lock); err != nil {
		return nil, fmt.Errorf("parse gateway image lock: %w", err)
	}
	image := resources.PublishedImage(gatewayImageName, "")
	if want := image.Repository + "/" + image.Name; lock.Name != want {
		return nil, fmt.Errorf("gateway image lock names %q, but this service's gateway image is %q", lock.Name, want)
	}
	if lock.Digest == "" {
		return nil, errGatewayImageUnpublished
	}
	if !isSHA256Digest(lock.Digest) {
		return nil, fmt.Errorf("gateway image digest %q must be a sha256 digest", lock.Digest)
	}
	image.Digest = lock.Digest
	return &image, nil
}

// isSHA256Digest reports whether digest is a complete sha256 image digest.
func isSHA256Digest(digest string) bool {
	algorithm, encoded, found := strings.Cut(digest, ":")
	decoded, err := hex.DecodeString(encoded)
	return found && algorithm == "sha256" && err == nil && len(decoded) == 32
}

// gatewayImageOverrideEnv names a locally built gateway image to run instead of
// the published one. It configures the agent, not the gateway: the gateway's own
// surface is the SWH_* variables internal/config reads.
const gatewayImageOverrideEnv = "SWH_GATEWAY_IMAGE"

// publishedGatewayImage is the image gateway-image.json pins, whatever the
// override says. A deployment, an audit and a source-scope inventory describe
// the image a release ships, which a locally built stand-in is not.
func publishedGatewayImage() (*resources.DockerImage, error) {
	return parseGatewayImageLock(gatewayImageLockJSON)
}

// effectiveGatewayImage resolves the gateway image the agent actually runs, and
// reports whether it came from the override. An override is built locally and
// never pushed, so it is reachable through the Docker daemon rather than a
// registry and carries the single platform the daemon holds.
//
// The published image is pinned to the digest gateway-image.json records. A tag
// serves whatever was pushed to it last, so an agent naming one would run, and
// report evidence for, an image nobody checked was the one built for this
// version.
func effectiveGatewayImage() (*resources.DockerImage, bool, error) {
	if override := os.Getenv(gatewayImageOverrideEnv); override != "" {
		return &resources.DockerImage{Name: override}, true, nil
	}
	image, err := publishedGatewayImage()
	return image, false, err
}
