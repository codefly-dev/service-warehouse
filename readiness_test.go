package main

import (
	"context"
	"net"
	"testing"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	grpchealth "google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"

	whv0 "github.com/codefly-dev/service-warehouse/gen/codefly/warehouse/v0"
	"github.com/codefly-dev/service-warehouse/internal/auth"
	"github.com/codefly-dev/service-warehouse/internal/backend"
	"github.com/codefly-dev/service-warehouse/internal/backend/mem"
	"github.com/codefly-dev/service-warehouse/internal/server"
)

// startGateway serves the gateway's gRPC surface — the Warehouse service behind
// the token interceptors, the health service outside them — on a loopback port
// and returns its address and its health server. cmd/service-warehouse builds
// the same server from the same pieces; it is rebuilt here because that
// constructor is in a main package.
func startGateway(t *testing.T, token string) (string, *grpchealth.Server) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	health := grpchealth.NewServer()
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(auth.UnaryInterceptor(token)),
		grpc.ChainStreamInterceptor(auth.StreamInterceptor(token)),
	)
	healthv1.RegisterHealthServer(srv, health)
	whv0.RegisterWarehouseServer(srv, server.New(mem.New(backend.Config{})))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String(), health
}

// runtimeAt wires a Runtime with just enough network state for WaitForReady to
// resolve address.
func runtimeAt(t *testing.T, address string, token string) *Runtime {
	t.Helper()
	rt := NewRuntime()
	endpoint := &basev0.Endpoint{Name: "grpc", Api: "grpc"}
	rt.GrpcEndpoint = endpoint
	rt.Runtime.WithContext(resources.NewRuntimeContextNative())
	rt.NetworkMappings = []*basev0.NetworkMapping{{
		Endpoint:  endpoint,
		Instances: []*basev0.NetworkInstance{{Address: address, Access: resources.NewNativeNetworkAccess()}},
	}}
	rt.gatewayToken = token
	return rt
}

// A readiness probe that swallowed its own error would retry a token mismatch
// for the whole budget and then report "not ready", pointing the operator at the
// gateway instead of at the credential.
func TestWaitForReadyReportsRejectedCredential(t *testing.T) {
	address, _ := startGateway(t, "server-token")
	rt := runtimeAt(t, address, "a-different-token")

	start := time.Now()
	err := rt.WaitForReady(context.Background())

	require.Error(t, err, "WaitForReady must fail when the gateway rejects the agent's credential")
	require.Contains(t, err.Error(), "rejected the agent's credential")
	require.Less(t, time.Since(start), 10*time.Second, "a rejected credential must not be retried as if transient")

	rt.gatewayToken = "server-token"
	require.NoError(t, rt.WaitForReady(context.Background()))
}

// The health service answers without a token, so a gateway that is up but
// reports itself not serving must not be called ready on the strength of
// Capabilities alone; once it serves, the wait ends.
func TestWaitForReadyWaitsForTheGatewayToServe(t *testing.T) {
	address, health := startGateway(t, "server-token")
	health.SetServingStatus("", healthv1.HealthCheckResponse_NOT_SERVING)
	rt := runtimeAt(t, address, "server-token")

	go func() {
		time.Sleep(1500 * time.Millisecond)
		health.SetServingStatus("", healthv1.HealthCheckResponse_SERVING)
	}()
	start := time.Now()
	require.NoError(t, rt.WaitForReady(context.Background()))
	require.GreaterOrEqual(t, time.Since(start), time.Second, "ready was reported while the gateway said it was not serving")
}

func TestProbeGatewayDistinguishesNotYetFromNever(t *testing.T) {
	address, health := startGateway(t, "server-token")
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecureCredentials()), auth.DialOption("server-token"))
	require.NoError(t, err)
	defer conn.Close()
	healthClient, warehouse := healthv1.NewHealthClient(conn), whv0.NewWarehouseClient(conn)

	reason, final := probeGateway(context.Background(), healthClient, warehouse)
	require.Empty(t, reason)
	require.NoError(t, final)

	health.SetServingStatus("", healthv1.HealthCheckResponse_NOT_SERVING)
	reason, final = probeGateway(context.Background(), healthClient, warehouse)
	require.Contains(t, reason, "NOT_SERVING")
	require.NoError(t, final, "a gateway that is not serving yet is worth retrying")
}

func TestWaitForReadyReturnsOnCancellation(t *testing.T) {
	rt := runtimeAt(t, "127.0.0.1:1", "token") // nothing listens here
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(1500 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := rt.WaitForReady(ctx)

	require.Error(t, err)
	require.Less(t, time.Since(start), 10*time.Second, "cancellation must end the wait, not the 60s budget")
}

func insecureCredentials() credentials.TransportCredentials { return insecure.NewCredentials() }
