package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/codefly-dev/core/resources"
)

// gatewayImageName is the repository, inside core's image registry, this
// service's gateway image is published to. The release workflows derive the same
// name from the repository they run in (ghcr.io/<owner>/<repo>), and
// TestGatewayImageNameIsThisRepository holds the two together.
const gatewayImageName = "service-warehouse"

// parseGatewayImageLock delegates lock validation to core, adding the service's
// recovery instruction when no image has been published yet.
func parseGatewayImageLock(content []byte) (*resources.DockerImage, error) {
	image, err := resources.ParseImageLock(content, gatewayImageName)
	if errors.Is(err, resources.ErrImageUnpublished) {
		return nil, fmt.Errorf("%w: run publish-gateway-image.yml for the upcoming version and commit the digest it prints", err)
	}
	return image, err
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
