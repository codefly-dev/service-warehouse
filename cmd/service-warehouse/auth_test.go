package main

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	whv0 "github.com/codefly-dev/service-warehouse/gen/codefly/warehouse/v0"
	"github.com/codefly-dev/service-warehouse/internal/auth"
	"github.com/codefly-dev/service-warehouse/internal/backend"
	"github.com/codefly-dev/service-warehouse/internal/backend/mem"
)

// serverToken is the secret the test gateway enforces.
const serverToken = "89d0d1f6d0e2f5a4c3b2a1908f7e6d5c"

// startGateway serves the server run() serves — built by the same newGRPCServer
// — over an in-memory listener and returns it with a way to dial it.
func startGateway(t *testing.T, token string) (*grpc.Server, func(...grpc.DialOption) *grpc.ClientConn) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := newGRPCServer(mem.New(backend.Config{}), token)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return srv, func(opts ...grpc.DialOption) *grpc.ClientConn {
		t.Helper()
		conn, err := grpc.NewClient("passthrough:///bufnet", append(opts,
			grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)...)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
}

// rpc is one method a peer could call, as the server itself registered it.
type rpc struct {
	name string
	call func(context.Context, *grpc.ClientConn) error
}

// registeredRPCs lists every method of every service the server registers
// except the health service, read from the server rather than from a list kept
// beside it: an RPC added to the proto, or a service added to the server, is
// covered by the refusal tests the moment it exists, without anyone remembering
// to extend them.
//
// Each call sends an empty message, which every request type decodes, so the
// outcome is decided by the interceptor alone. A call that gets past it ends in
// whatever status its handler gives an empty request; that is not asserted here.
func registeredRPCs(t *testing.T, srv *grpc.Server) []rpc {
	t.Helper()
	var rpcs []rpc
	for service, info := range srv.GetServiceInfo() {
		for _, m := range info.Methods {
			full := "/" + service + "/" + m.Name
			if auth.IsHealthMethod(full) {
				continue
			}
			rpcs = append(rpcs, rpc{name: full, call: caller(full, m)})
		}
	}
	require.NotEmpty(t, rpcs)
	return rpcs
}

func caller(full string, m grpc.MethodInfo) func(context.Context, *grpc.ClientConn) error {
	if !m.IsClientStream && !m.IsServerStream {
		return func(ctx context.Context, conn *grpc.ClientConn) error {
			return conn.Invoke(ctx, full, &emptypb.Empty{}, &emptypb.Empty{})
		}
	}
	return func(ctx context.Context, conn *grpc.ClientConn) error {
		st, err := conn.NewStream(ctx, &grpc.StreamDesc{
			ServerStreams: m.IsServerStream,
			ClientStreams: m.IsClientStream,
		}, full)
		if err != nil {
			return err
		}
		// A refused stream makes SendMsg fail with io.EOF; RecvMsg then carries
		// the real status.
		if err := st.SendMsg(&emptypb.Empty{}); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if err := st.CloseSend(); err != nil {
			return err
		}
		for {
			if err := st.RecvMsg(&emptypb.Empty{}); err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
	}
}

// TestGatewayCoversTheWholeContract pins what the two tests below enumerate: the
// Warehouse service as the proto declares it, the streaming methods included.
// Without it, those tests would still pass if the enumeration quietly saw less
// than the server serves.
func TestGatewayCoversTheWholeContract(t *testing.T) {
	srv, _ := startGateway(t, serverToken)

	prefix := "/" + whv0.Warehouse_ServiceDesc.ServiceName + "/"
	declared := len(whv0.Warehouse_ServiceDesc.Methods) + len(whv0.Warehouse_ServiceDesc.Streams)
	var covered int
	for _, r := range registeredRPCs(t, srv) {
		if strings.HasPrefix(r.name, prefix) {
			covered++
		}
	}
	require.Equal(t, declared, covered)
	require.NotEmpty(t, whv0.Warehouse_ServiceDesc.Streams, "the contract has streaming methods, which take the stream interceptor")
}

// TestUnauthenticatedPeerIsRefused is the core guarantee: a peer that reaches
// the port without the deployment's token can run nothing — over unary and
// streaming RPCs alike.
func TestUnauthenticatedPeerIsRefused(t *testing.T) {
	srv, dial := startGateway(t, serverToken)

	peers := map[string]*grpc.ClientConn{
		"no credentials":   dial(),
		"wrong token":      dial(auth.DialOption("00000000000000000000000000000000")),
		"prefix of token":  dial(auth.DialOption(serverToken[:len(serverToken)-1])),
		"token plus extra": dial(auth.DialOption(serverToken + "x")),
		"empty token":      dial(auth.DialOption("")),
	}

	for peerName, conn := range peers {
		for _, r := range registeredRPCs(t, srv) {
			t.Run(peerName+"/"+r.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				err := r.call(ctx, conn)
				require.Error(t, err)
				require.Equal(t, codes.Unauthenticated, status.Code(err), "got %v", err)
			})
		}
	}
}

// TestAuthorizedPeerReachesTheHandler pairs with the refusal test: the same
// calls must get through once the token is presented, or the gateway would be
// locked shut rather than protected. A handler-level status (InvalidArgument,
// Unimplemented) still proves the interceptor let the call through.
func TestAuthorizedPeerReachesTheHandler(t *testing.T) {
	srv, dial := startGateway(t, serverToken)
	conn := dial(auth.DialOption(serverToken))

	for _, r := range registeredRPCs(t, srv) {
		t.Run(r.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := r.call(ctx, conn)
			require.NotEqual(t, codes.Unauthenticated, status.Code(err), "got %v", err)
		})
	}
}

// TestTokenTravelsUnderTheEcosystemKey pins the wire contract: the gateway reads
// the same metadata key the codefly host uses for agent plugins, so a caller
// built against that convention interoperates.
func TestTokenTravelsUnderTheEcosystemKey(t *testing.T) {
	require.Equal(t, "x-codefly-token", auth.MetadataKey)

	_, dial := startGateway(t, serverToken)
	conn := dial()
	ctx := metadata.AppendToOutgoingContext(context.Background(), "x-codefly-token", serverToken)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := whv0.NewWarehouseClient(conn).Capabilities(ctx, &whv0.CapabilitiesRequest{})
	require.NoError(t, err)
}

// TestTokenUnderAnotherHeaderIsRefused guards against a caller that presents the
// right secret the wrong way: an HTTP-style Authorization bearer is not the
// gateway's credential and must not be accepted.
func TestTokenUnderAnotherHeaderIsRefused(t *testing.T) {
	_, dial := startGateway(t, serverToken)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+serverToken)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err := whv0.NewWarehouseClient(dial()).Capabilities(ctx, &whv0.CapabilitiesRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err), "got %v", err)
}

// TestHealthNeedsNoToken pins the one exemption: the standard gRPC health
// service answers a peer that presents no token. A readiness probe — the Codefly
// CLI's, a load balancer's, a mesh's — carries none, and a guarded health service
// makes the gateway look permanently unready to all of them. Every data RPC stays
// refused; TestUnauthenticatedPeerIsRefused holds that side.
func TestHealthNeedsNoToken(t *testing.T) {
	_, dial := startGateway(t, serverToken)
	health := healthv1.NewHealthClient(dial())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := health.Check(ctx, &healthv1.HealthCheckRequest{})
	require.NoError(t, err)
	require.Equal(t, healthv1.HealthCheckResponse_SERVING, resp.GetStatus())

	stream, err := health.Watch(ctx, &healthv1.HealthCheckRequest{})
	require.NoError(t, err)
	first, err := stream.Recv()
	require.NoError(t, err)
	require.Equal(t, healthv1.HealthCheckResponse_SERVING, first.GetStatus())
}

// TestWithoutATokenTheListenerIsAnonymous is the other side of the config guard:
// config.FromEnv resolves an empty token only when the operator chose
// SWH_ALLOW_ANONYMOUS, and then the server must take callers with no credential
// rather than refuse everyone.
func TestWithoutATokenTheListenerIsAnonymous(t *testing.T) {
	_, dial := startGateway(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := whv0.NewWarehouseClient(dial()).Capabilities(ctx, &whv0.CapabilitiesRequest{})
	require.NoError(t, err)
}
