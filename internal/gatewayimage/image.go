// Package gatewayimage declares the gateway's shipped platforms and role.
// Core owns subject derivation, scanning, and evidence writing.
package gatewayimage

// Platforms is held to the publish workflow's explicit platform input by tests.
var Platforms = []string{"linux/amd64", "linux/arm64"}

const Role = "runtime"
