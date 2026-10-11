package mcpserver

import (
	"context"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type shipIn struct {
	Resource    string `json:"resource" jsonschema:"resource to copy"`
	Target      string `json:"target" jsonschema:"the other cluster's controller, host:port"`
	TargetToken string `json:"target_token,omitempty" jsonschema:"an API token of the other cluster, when it requires one"`
	Name        string `json:"name,omitempty" jsonschema:"name there; default the same"`
	Pool        string `json:"pool,omitempty" jsonschema:"pool there; default the same name as here"`
	Replicas    uint32 `json:"replicas,omitempty" jsonschema:"replicas there; default as many as here"`
}

func (s *Server) registerShipTools(srv *mcp.Server) {
	c, supported := s.client.(interface {
		ShipResource(context.Context, *haifypb.ShipResourceRequest) (*haifypb.StorageJobResponse, error)
	})
	if !supported {
		return
	}
	addWrite(s, srv, writeTool("haify_resource_ship", "Copy a resource to another cluster",
		"Copy a resource to another Haify cluster directly: a snapshot of every volume streams, encrypted, from a "+
			"replica here into a new resource there, and both ends checksum it. The resource keeps serving. Refused "+
			"when the name is taken there, and for encrypted or ZFS-backed resources. Returns a job id; follow it "+
			"with haify_storage_jobs."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in shipIn) (*mcp.CallToolResult, jobStartOut, error) {
			resp, err := c.ShipResource(ctx, &haifypb.ShipResourceRequest{Resource: in.Resource, Target: in.Target,
				TargetToken: in.TargetToken, Name: in.Name, Pool: in.Pool, Replicas: in.Replicas})
			if err != nil {
				return nil, jobStartOut{}, err
			}
			return nil, jobStartOut{JobID: resp.JobId, Detail: resp.Message}, nil
		})
}
