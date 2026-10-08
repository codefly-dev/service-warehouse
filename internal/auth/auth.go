// Package auth authenticates callers of the gateway's gRPC surface: does the
// caller present the one token this deployment was started with. It decides
// nothing else — which caller may run which statement is settled before the
// request arrives (see AGENTS.md), so there is no identity here, no
// per-method rule, and no statement inspection.
//
// It is the same mechanism codefly-dev/service-object-storage uses
// (internal/auth) and the one the Codefly host already uses to talk to agent
// plugins: a shared secret carried as the "x-codefly-token" metadata header,
// compared in constant time. No exported equivalent exists to import:
// codefly-dev/core exports only the key (agents.AuthMetadataKey, in the
// package that links the whole agent runtime) and keeps its interceptors
// unexported, and codefly-dev/sdk-go has none. The key is therefore redeclared
// here, and this package is a copy of service-object-storage's; a fix to one
// belongs in the other.
package auth

import (
	"context"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// MetadataKey is the gRPC metadata key carrying the token. Lowercase per gRPC
// convention — metadata keys are case-insensitive but the wire form is
// lowercase.
const MetadataKey = "x-codefly-token"

// HealthServicePrefix is the standard gRPC health service. Its Check and Watch
// carry no data and no authority: a readiness probe — the Codefly CLI's, a
// load balancer's, a mesh's — presents no token, so a guarded health service
// makes the gateway look permanently unready to everything that would route to
// it. Every other method stays behind the token.
const HealthServicePrefix = "/grpc.health.v1.Health/"

// IsHealthMethod reports whether fullMethod belongs to the gRPC health service.
func IsHealthMethod(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, HealthServicePrefix)
}

// UnaryInterceptor rejects every unary call that does not present token.
func UnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !IsHealthMethod(info.FullMethod) {
			if err := verify(ctx, token); err != nil {
				return nil, err
			}
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor rejects every streaming call that does not present token.
// gRPC runs it before the handler observes a single frame, so an unauthorized
// InsertRows is refused at stream open rather than after its first batch is
// accepted.
func StreamInterceptor(token string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !IsHealthMethod(info.FullMethod) {
			if err := verify(ss.Context(), token); err != nil {
				return err
			}
		}
		return handler(srv, ss)
	}
}

// DialOption is how a client presents token on every call it makes to the
// gateway.
func DialOption(token string) grpc.DialOption {
	return grpc.WithPerRPCCredentials(bearer(token))
}

func verify(ctx context.Context, expected string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing "+MetadataKey)
	}
	presented := md.Get(MetadataKey)
	// An empty presented value would compare equal to an empty expected one, so
	// it is refused as absent rather than reaching the comparison.
	if len(presented) == 0 || presented[0] == "" {
		return status.Error(codes.Unauthenticated, "missing "+MetadataKey)
	}
	if subtle.ConstantTimeCompare([]byte(presented[0]), []byte(expected)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid "+MetadataKey)
	}
	return nil
}

// bearer carries the token as per-RPC credentials.
type bearer string

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{MetadataKey: string(b)}, nil
}

// RequireTransportSecurity is false: the local profile reaches the gateway over
// the Docker host bridge without TLS, and the deployed profile terminates
// transport security at the mesh. Demanding it here would make the credential
// unusable on both.
func (b bearer) RequireTransportSecurity() bool { return false }
