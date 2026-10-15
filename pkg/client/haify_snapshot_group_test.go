package client

import (
	"context"
	"net"
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type snapshotGroupServer struct {
	haifypb.UnimplementedHaifyControllerServer
	got []*haifypb.SnapshotGroupMember
}

func (s *snapshotGroupServer) CreateSnapshotGroup(_ context.Context, req *haifypb.CreateSnapshotGroupRequest) (*haifypb.CreateSnapshotGroupResponse, error) {
	s.got = req.GetMembers()
	if len(req.GetMembers()) > 0 && req.GetMembers()[0].GetSnapshotName() == "fail" {
		return &haifypb.CreateSnapshotGroupResponse{Success: false, Message: "no space"}, nil
	}
	return &haifypb.CreateSnapshotGroupResponse{Success: true}, nil
}

func TestCreateSnapshotGroup(t *testing.T) {
	lis := bufconn.Listen(bufSize)
	srv := &snapshotGroupServer{}
	s := grpc.NewServer()
	haifypb.RegisterHaifyControllerServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	defer s.Stop()
	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	c := &HaifyClient{conn: conn, client: haifypb.NewHaifyControllerClient(conn)}

	members := []*haifypb.SnapshotGroupMember{
		{Resource: "a", Volume: "vg0/a_data", SnapshotName: "g_1", Node: "n1"},
		{Resource: "b", Volume: "vg0/b_data", SnapshotName: "g_2", Node: "n2"},
	}
	require.NoError(t, c.CreateSnapshotGroup(context.Background(), members))
	require.Len(t, srv.got, 2)
	assert.Equal(t, "vg0/b_data", srv.got[1].GetVolume())

	err = c.CreateSnapshotGroup(context.Background(), []*haifypb.SnapshotGroupMember{{SnapshotName: "fail"}})
	assert.EqualError(t, err, "no space")
}
