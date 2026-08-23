package server

import (
	"context"
	"net"
	"testing"

	whv0 "github.com/codefly-dev/service-warehouse/gen/codefly/warehouse/v0"
	"github.com/codefly-dev/service-warehouse/internal/backend"
	"github.com/codefly-dev/service-warehouse/internal/backend/mem"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// dial spins up the Warehouse service over an in-memory listener backed by the
// mem backend and returns a connected client.
func dial(t *testing.T) whv0.WarehouseClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	whv0.RegisterWarehouseServer(srv, New(mem.New(backend.Config{})))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return whv0.NewWarehouseClient(conn)
}

func TestServerCatalogRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := dial(t)

	_, err := c.CreateDataset(ctx, &whv0.CreateDatasetRequest{Name: "analytics"})
	require.NoError(t, err)

	ref := &whv0.TableRef{Dataset: "analytics", Table: "events"}
	_, err = c.CreateTable(ctx, &whv0.CreateTableRequest{
		Ref:     ref,
		Columns: []*whv0.Column{{Name: "id", Type: whv0.ColumnType_COLUMN_TYPE_INT64}},
	})
	require.NoError(t, err)

	sc, err := c.GetTable(ctx, &whv0.GetTableRequest{Ref: ref})
	require.NoError(t, err)
	require.Equal(t, "id", sc.GetColumns()[0].GetName())
	require.Equal(t, whv0.ColumnType_COLUMN_TYPE_INT64, sc.GetColumns()[0].GetType())

	ds, err := c.ListDatasets(ctx, &whv0.ListDatasetsRequest{})
	require.NoError(t, err)
	require.Len(t, ds.GetDatasets(), 1)
}

func TestServerQueryUnimplemented(t *testing.T) {
	c := dial(t)
	stream, err := c.Query(context.Background(), &whv0.QueryRequest{Sql: "SELECT 1"})
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestServerCapabilities(t *testing.T) {
	c := dial(t)
	caps, err := c.Capabilities(context.Background(), &whv0.CapabilitiesRequest{})
	require.NoError(t, err)
	require.Equal(t, "mem", caps.GetBackend())
	require.True(t, caps.GetDdl())
}
