package client

import (
	"context"
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// AppCreateRequest asks for a database application. Resource defaults to
// Name and Port to the engine's own.
type AppCreateRequest struct {
	Name      string
	Engine    string // postgres, mysql, redis or rustfs
	Resource  string
	ServiceIP string // CIDR
	Port      uint32
	Vector    bool // postgres only: pgvector
}

// CreateApp creates a database application. The response carries the
// generated password, which is returned by this call only.
func (c *HaifyClient) CreateApp(ctx context.Context, req AppCreateRequest) (*haifypb.CreateAppResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.CreateApp: sending request", zap.String("name", req.Name), zap.String("engine", req.Engine),
		zap.String("resource", req.Resource), zap.String("service_ip", req.ServiceIP), zap.Uint32("port", req.Port), zap.Bool("vector", req.Vector))
	resp, err := c.client.CreateApp(ctx, &haifypb.CreateAppRequest{
		Name:      req.Name,
		Engine:    req.Engine,
		Resource:  req.Resource,
		ServiceIp: req.ServiceIP,
		Port:      req.Port,
		Vector:    req.Vector,
	})
	if err != nil {
		log.Debug("HaifyClient.CreateApp: CreateApp failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.CreateApp: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.CreateApp: done")
	return resp, nil
}

// ListApps lists the database applications.
func (c *HaifyClient) ListApps(ctx context.Context) ([]*haifypb.AppInfo, error) {
	log := zap.L()
	log.Debug("HaifyClient.ListApps: sending request")
	resp, err := c.client.ListApps(ctx, &haifypb.ListAppsRequest{})
	if err != nil {
		log.Debug("HaifyClient.ListApps: ListApps failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.ListApps: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.ListApps: done", zap.Int("count", len(resp.Apps)))
	return resp.Apps, nil
}

// GetAppStatus reports where an app runs and whether it answers there.
func (c *HaifyClient) GetAppStatus(ctx context.Context, name string) (*haifypb.GetAppStatusResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.GetAppStatus: sending request", zap.String("name", name))
	resp, err := c.client.GetAppStatus(ctx, &haifypb.GetAppStatusRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.GetAppStatus: GetAppStatus failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.GetAppStatus: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.GetAppStatus: done")
	return resp, nil
}

// DeleteApp removes an app; with deleteData its resource is deleted too.
func (c *HaifyClient) DeleteApp(ctx context.Context, name string, deleteData bool) (string, error) {
	log := zap.L()
	log.Debug("HaifyClient.DeleteApp: sending request", zap.String("name", name), zap.Bool("delete_data", deleteData))
	resp, err := c.client.DeleteApp(ctx, &haifypb.DeleteAppRequest{Name: name, DeleteData: deleteData})
	if err != nil {
		log.Debug("HaifyClient.DeleteApp: DeleteApp failed", zap.Error(err))
		return "", err
	}
	if !resp.Success {
		log.Debug("HaifyClient.DeleteApp: controller reported failure", zap.String("message", resp.Message))
		return "", fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.DeleteApp: done")
	return resp.Message, nil
}

// FailoverApp moves an app to another replica.
func (c *HaifyClient) FailoverApp(ctx context.Context, name string) (*haifypb.FailoverAppResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.FailoverApp: sending request", zap.String("name", name))
	resp, err := c.client.FailoverApp(ctx, &haifypb.FailoverAppRequest{Name: name})
	if err != nil {
		log.Debug("HaifyClient.FailoverApp: FailoverApp failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.FailoverApp: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.FailoverApp: done")
	return resp, nil
}

// SnapshotApp takes a resource snapshot of an app with the database frozen.
func (c *HaifyClient) SnapshotApp(ctx context.Context, name, snapshot string) (*haifypb.SnapshotAppResponse, error) {
	log := zap.L()
	log.Debug("HaifyClient.SnapshotApp: sending request", zap.String("name", name), zap.String("snapshot", snapshot))
	resp, err := c.client.SnapshotApp(ctx, &haifypb.SnapshotAppRequest{Name: name, Snapshot: snapshot})
	if err != nil {
		log.Debug("HaifyClient.SnapshotApp: SnapshotApp failed", zap.Error(err))
		return nil, err
	}
	if !resp.Success {
		log.Debug("HaifyClient.SnapshotApp: controller reported failure", zap.String("message", resp.Message))
		return nil, fmt.Errorf("%s", resp.Message)
	}
	log.Debug("HaifyClient.SnapshotApp: done")
	return resp, nil
}
