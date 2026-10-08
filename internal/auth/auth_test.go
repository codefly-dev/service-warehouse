package auth_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/codefly-dev/service-warehouse/internal/auth"
)

const token = "89d0d1f6d0e2f5a4c3b2a1908f7e6d5c"

// peerContext is the context a handler sees for a call whose metadata carries
// the given header values; none means the call carried no metadata at all.
func peerContext(values ...string) context.Context {
	if len(values) == 0 {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(auth.MetadataKey, values[0]))
}

// serverStream is the least a stream interceptor needs of a ServerStream: the
// context the call arrived with.
type serverStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s serverStream) Context() context.Context { return s.ctx }

// callUnary runs one unary call through the interceptor and reports whether
// the handler was reached and what the caller was told.
func callUnary(ctx context.Context, method string) (reached bool, err error) {
	_, err = auth.UnaryInterceptor(token)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) {
			reached = true
			return nil, nil
		})
	return reached, err
}

func callStream(ctx context.Context, method string) (reached bool, err error) {
	err = auth.StreamInterceptor(token)(nil, serverStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: method},
		func(any, grpc.ServerStream) error {
			reached = true
			return nil
		})
	return reached, err
}

const dataMethod = "/codefly.warehouse.v0.Warehouse/Query"

// Every way of not presenting the token is refused with Unauthenticated before
// the handler runs, on both interceptors. The near misses matter most: a
// comparison that stopped at the shorter length, or that accepted an empty
// value against an empty expectation, would pass an obvious wrong-token test.
func TestOnlyTheExactTokenReachesTheHandler(t *testing.T) {
	refused := map[string][]string{
		"no metadata":      nil,
		"empty value":      {""},
		"wrong token":      {"00000000000000000000000000000000"},
		"prefix of token":  {token[:len(token)-1]},
		"token plus extra": {token + "x"},
		"different case":   {"89D0D1F6D0E2F5A4C3B2A1908F7E6D5C"},
	}
	for name, values := range refused {
		t.Run(name, func(t *testing.T) {
			reached, err := callUnary(peerContext(values...), dataMethod)
			require.False(t, reached, "the handler must not run for a refused unary call")
			require.Equal(t, codes.Unauthenticated, status.Code(err), "got %v", err)

			reached, err = callStream(peerContext(values...), dataMethod)
			require.False(t, reached, "the handler must not run for a refused stream")
			require.Equal(t, codes.Unauthenticated, status.Code(err), "got %v", err)
		})
	}

	reached, err := callUnary(peerContext(token), dataMethod)
	require.NoError(t, err)
	require.True(t, reached)

	reached, err = callStream(peerContext(token), dataMethod)
	require.NoError(t, err)
	require.True(t, reached)
}

// The refusal says which header was wrong, not what the right one is.
func TestRefusalNeverEchoesTheExpectedToken(t *testing.T) {
	_, err := callUnary(peerContext("wrong"), dataMethod)
	require.Error(t, err)
	require.NotContains(t, err.Error(), token)
}

// The health service is the one exemption: a readiness probe carries no token.
// Anything that merely looks like it does not inherit it.
func TestOnlyTheHealthServiceIsExempt(t *testing.T) {
	for _, method := range []string{"/grpc.health.v1.Health/Check", "/grpc.health.v1.Health/Watch"} {
		t.Run(method, func(t *testing.T) {
			require.True(t, auth.IsHealthMethod(method))

			reached, err := callUnary(peerContext(), method)
			require.NoError(t, err)
			require.True(t, reached)

			reached, err = callStream(peerContext(), method)
			require.NoError(t, err)
			require.True(t, reached)
		})
	}

	for _, method := range []string{
		"/grpc.health.v1.HealthX/Check",
		"/grpc.health.v1.Healthy/Check",
		"/other.Service/grpc.health.v1.Health/Check",
		"/codefly.warehouse.v0.Warehouse/Health",
		"grpc.health.v1.Health/Check",
		"",
	} {
		t.Run("not "+method, func(t *testing.T) {
			require.False(t, auth.IsHealthMethod(method))

			reached, err := callUnary(peerContext(), method)
			require.False(t, reached)
			require.Equal(t, codes.Unauthenticated, status.Code(err), "got %v", err)
		})
	}
}
