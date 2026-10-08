//go:build e2e

// This file drives the agent Runtime through its real lifecycle — Load → Init →
// Start — standing the gateway container up exactly as `codefly run` does, then
// calls it over the advertised gRPC endpoint with the credential the agent
// handed out. Run with:
//
//	docker build -t service-warehouse:e2e .
//	SWH_GATEWAY_IMAGE=service-warehouse:e2e go test -tags e2e -run TestRuntimeEndToEnd .
//
// The gateway image is a prerequisite because the published one only exists once
// a release has run; the test points the Runtime at the locally built image
// through the same SWH_GATEWAY_IMAGE override the Runtime already honors.
//
// It asks the gateway for the catalog-only backend: that is the one a container
// can serve with no warehouse, so what is under test is the agent, the image and
// the credential, not a warehouse.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	whv0 "github.com/codefly-dev/service-warehouse/gen/codefly/warehouse/v0"
	"github.com/codefly-dev/service-warehouse/internal/auth"
)

func TestRuntimeEndToEnd(t *testing.T) {
	if os.Getenv(gatewayImageOverrideEnv) == "" {
		t.Skipf("%s not set; skipping agent-runtime end-to-end test", gatewayImageOverrideEnv)
	}
	ctx := context.Background()
	rt, networkMappings, runtimeContext := loadedRuntime(t, ctx)

	// Register teardown before Init: a failure after the container exists must
	// still clean it up. Destroy nil-checks the environment, so it is safe even
	// if Init failed before starting anything. The flag keeps this safety net
	// from running a second Shutdown after the explicit teardown asserted below.
	destroyed := false
	defer func() {
		if !destroyed {
			_, _ = rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
		}
	}()

	init, err := rt.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          runtimeContext,
		ProposedNetworkMappings: networkMappings,
		Configuration:           configuration(map[string]string{"SWH_BACKEND": "mem"}),
	})
	require.NoError(t, err)
	// Init reports a lifecycle failure in its status, not as a returned error:
	// the contract hands the CLI a diagnostic rather than a transport fault.
	require.Equal(t, runtimev0.InitStatus_READY, init.GetStatus().GetState(), init.GetStatus().GetMessage())

	start, err := rt.Start(ctx, &runtimev0.StartRequest{})
	require.NoError(t, err)
	require.Equal(t, runtimev0.StartStatus_STARTED, start.GetStatus().GetState(), start.GetStatus().GetMessage())

	// A consumer running in its own container reaches the gateway through
	// host.docker.internal, which resolves to the bridge gateway on Linux — a
	// 127.0.0.1-bound port is unreachable there. It must publish on all
	// interfaces; the token checked below is what keeps that from being an open
	// door.
	gatewayID, err := rt.gatewayEnv.ContainerID()
	require.NoError(t, err)
	requirePublishedOnAllInterfaces(t, gatewayID, gatewayContainerPort)

	conf, err := resources.ExtractConfiguration(init.RuntimeConfigurations, resources.NewRuntimeContextNative())
	require.NoError(t, err)
	endpoint, err := resources.GetConfigurationValue(ctx, conf, configurationGroup, "endpoint")
	require.NoError(t, err)
	require.NotEmpty(t, endpoint)
	connection, err := resources.GetConfigurationValue(ctx, conf, configurationGroup, "connection")
	require.NoError(t, err)
	token, err := resources.GetConfigurationValue(ctx, conf, configurationGroup, "token")
	require.NoError(t, err)
	require.NotEmpty(t, token, "the runtime must hand consumers a gateway credential")
	require.NotContains(t, endpoint, token, "endpoint must not carry the credential")
	require.NotContains(t, connection, token, "connection string must not carry the credential")
	requireTokenIsSecret(t, conf, token)

	requireUnauthenticatedPeerIsRefused(t, endpoint)

	// What the gateway was started with is what the agent resolved: the backend
	// it was told, and no variable for a value nobody set.
	env, err := exec.Command("docker", "inspect", "-f", `{{join .Config.Env "\n"}}`, gatewayID).Output()
	require.NoError(t, err)
	gatewayEnv := strings.Split(string(env), "\n")
	require.Contains(t, gatewayEnv, "SWH_BACKEND=mem")
	require.Contains(t, gatewayEnv, fmt.Sprintf("SWH_LISTEN=:%d", gatewayContainerPort))
	for _, variable := range gatewayEnv {
		require.False(t, strings.HasPrefix(variable, "SWH_DATABASE="), "a value nobody set must be left to the gateway: %s", variable)
	}

	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		auth.DialOption(token))
	require.NoError(t, err)
	defer conn.Close()
	capabilities, err := whv0.NewWarehouseClient(conn).Capabilities(ctx, &whv0.CapabilitiesRequest{})
	require.NoError(t, err)
	require.NotNil(t, capabilities)

	// Teardown owns exactly what it started.
	_, err = rt.Destroy(context.Background(), &runtimev0.DestroyRequest{})
	require.NoError(t, err)
	destroyed = true
	require.Error(t, exec.Command("docker", "inspect", gatewayID).Run(), "container %s survived teardown", gatewayID)
}

// requireTokenIsSecret asserts the gateway credential is carried as a
// secret-marked configuration value, so nothing downstream logs or renders it.
func requireTokenIsSecret(t *testing.T, conf *basev0.Configuration, token string) {
	t.Helper()
	for _, info := range conf.GetInfos() {
		for _, value := range info.GetConfigurationValues() {
			if value.GetKey() == "token" {
				require.Equal(t, token, value.GetValue())
				require.True(t, value.GetSecret(), "gateway token must be marked secret")
				return
			}
		}
	}
	t.Fatal("no token value in the runtime configuration")
}

// requireUnauthenticatedPeerIsRefused dials the published gateway port the way
// any other host on the network could — no credential at all, then a plausible
// wrong one — and asserts the gateway refuses it. This is the acceptance case
// for the all-interface binding asserted above: reachable, but useless without
// the session token.
func requireUnauthenticatedPeerIsRefused(t *testing.T, endpoint string) {
	t.Helper()
	peers := map[string][]grpc.DialOption{
		"no credentials": nil,
		"wrong token":    {auth.DialOption("0000000000000000000000000000000000000000000000000000000000000000")},
	}
	for peerName, extra := range peers {
		conn, err := grpc.NewClient(endpoint,
			append(extra, grpc.WithTransportCredentials(insecure.NewCredentials()))...)
		require.NoError(t, err)
		t.Run(peerName, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := whv0.NewWarehouseClient(conn).Capabilities(ctx, &whv0.CapabilitiesRequest{})
			require.Error(t, err)
			require.Equal(t, codes.Unauthenticated, status.Code(err), "got %v", err)
		})
		_ = conn.Close()
	}
}

// loadedRuntime scaffolds a service through the Builder and returns a Runtime
// loaded against it, with the network mappings and runtime context Init needs.
//
// The native runtime context puts the test process on the host, so both the
// Runtime's readiness probe and the test reach the gateway at localhost:<hostPort>.
func loadedRuntime(t *testing.T, ctx context.Context) (*Runtime, []*basev0.NetworkMapping, *basev0.RuntimeContext) {
	t.Helper()

	// Keep all persistent test data outside the developer's real Codefly home.
	codeflyHome, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv(resources.CodeflyHomeEnv, codeflyHome)
	workspace := &resources.Workspace{Name: "test"}
	tmpDir := t.TempDir()
	serviceName := fmt.Sprintf("svc-%v", time.Now().UnixMilli())
	service := resources.Service{Name: serviceName, Version: "test-me"}
	require.NoError(t, service.SaveAtDir(ctx, path.Join(tmpDir, "mod", service.Name)))

	identity := &basev0.ServiceIdentity{
		Name:                service.Name,
		Version:             service.Version,
		Module:              "mod",
		Workspace:           workspace.Name,
		WorkspacePath:       tmpDir,
		RelativeToWorkspace: fmt.Sprintf("mod/%s", service.Name),
	}

	builder := NewBuilder()
	_, err = builder.Load(ctx, &builderv0.LoadRequest{
		DisableCatch: true,
		Identity:     identity,
		CreationMode: &builderv0.CreationMode{Communicate: false},
	})
	require.NoError(t, err)
	_, err = builder.Create(ctx, &builderv0.CreateRequest{})
	require.NoError(t, err)

	rt := NewRuntime()
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	require.NoError(t, err)
	networkManager.WithTemporaryPorts()
	env := resources.LocalEnvironment()

	_, err = rt.Load(ctx, &runtimev0.LoadRequest{
		Identity:     identity,
		Environment:  shared.Must(env.Proto()),
		DisableCatch: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, len(rt.Endpoints))

	runtimeContext := resources.NewRuntimeContextNative()
	networkMappings, err := networkManager.GenerateNetworkMappings(ctx, env, workspace, rt.Identity, rt.Endpoints, runtimeContext)
	require.NoError(t, err)

	return rt, networkMappings, runtimeContext
}

// requirePublishedOnAllInterfaces asserts the container's port is published on
// 0.0.0.0, not 127.0.0.1 — the difference between reachable and refused from a
// consumer container on a Linux bridge network.
//
// Docker can publish a single port under more than one host binding (an IPv6
// "::" entry beside the IPv4 "0.0.0.0" one), and their order is not guaranteed,
// so it ranges over every binding rather than trusting index 0.
func requirePublishedOnAllInterfaces(t *testing.T, containerID string, containerPort int) {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f",
		fmt.Sprintf(`{{range (index .NetworkSettings.Ports "%d/tcp")}}{{.HostIp}} {{end}}`, containerPort),
		containerID).Output()
	require.NoError(t, err)
	hostIPs := strings.Fields(string(out))
	require.NotEmpty(t, hostIPs, "container port %d has no published host binding", containerPort)
	require.NotContains(t, hostIPs, "127.0.0.1", "container port %d must not be published loopback-only", containerPort)
	require.Contains(t, hostIPs, "0.0.0.0", "container port %d must publish on all interfaces", containerPort)
}
