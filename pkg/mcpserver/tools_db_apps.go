package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/haify-project/haify/pkg/client"
)

// Database applications on bare metal (`haify app`): one database instance on a
// resource's DRBD volume, failed over by drbd-reactor. The Kubernetes
// equivalent is the haify_k8s_app_* set on the haify-k8s server (tools_apps.go).
//
// haify_app_create never returns the generated password. Whatever a tool
// returns lands in the conversation that called it, and from there in logs
// and transcripts nobody audits as credential stores. The tool says where the
// password is kept instead: root-only, on the volume, on the node running
// the database. `haify app create` at the CLI prints it once.

type dbAppCreateIn struct {
	Name      string `json:"name" jsonschema:"app name: 1-40 lower-case letters, digits and inner hyphens"`
	Engine    string `json:"engine" jsonschema:"engine: postgres, mysql (MySQL or MariaDB, whichever the nodes have), redis, or rustfs (an S3-compatible object store)"`
	Resource  string `json:"resource,omitempty" jsonschema:"existing resource with at least two diskful replicas (default: the app name)"`
	ServiceIP string `json:"service_ip" jsonschema:"service IP clients connect to, IPv4 in CIDR notation, e.g. 192.168.1.60/24"`
	Port      uint32 `json:"port,omitempty" jsonschema:"TCP port (default 5432, 3306, 6379 or 9000; rustfs also takes the next port for its console)"`
	Vector    bool   `json:"vector,omitempty" jsonschema:"postgres only: install the pgvector extension"`
}

type dbAppNameIn struct {
	Name string `json:"name" jsonschema:"app name"`
}

type dbAppDeleteIn struct {
	Name       string `json:"name" jsonschema:"app name"`
	DeleteData bool   `json:"delete_data,omitempty" jsonschema:"also delete the resource and every byte of its data"`
}

type dbAppSnapshotIn struct {
	Name     string `json:"name" jsonschema:"app name"`
	Snapshot string `json:"snapshot" jsonschema:"snapshot name: letters, digits, _ and -, at most 40"`
}

type dbAppOut struct {
	Name            string `json:"name"`
	Engine          string `json:"engine"`
	Resource        string `json:"resource"`
	ServiceIP       string `json:"service_ip"`
	Port            uint32 `json:"port"`
	Vector          bool   `json:"vector,omitempty"`
	Version         string `json:"version,omitempty"`
	AdminUser       string `json:"admin_user" jsonschema:"database account whose password was generated"`
	CredentialsFile string `json:"credentials_file" jsonschema:"where the password is kept: root-only, on the volume, on the node running the app"`
	Connection      string `json:"connection" jsonschema:"how a client reaches it through the service IP"`
}

type dbAppListOut struct {
	Apps []dbAppOut `json:"apps"`
}

type dbAppStatusOut struct {
	dbAppOut
	State        string   `json:"state" jsonschema:"running, degraded (Primary somewhere but the database is not active or does not answer) or stopped"`
	PrimaryNode  string   `json:"primary_node,omitempty" jsonschema:"node the app runs on"`
	ServiceState string   `json:"service_state,omitempty" jsonschema:"systemctl is-active of the app's unit there"`
	Healthy      bool     `json:"healthy" jsonschema:"the engine's health probe answered"`
	Nodes        []string `json:"nodes" jsonschema:"replicas the app can fail over to"`
}

func dbAppFrom(a *haifypb.AppInfo) dbAppOut {
	return dbAppOut{Name: a.GetName(), Engine: a.GetEngine(), Resource: a.GetResource(), ServiceIP: a.GetServiceIp(),
		Port: a.GetPort(), Vector: a.GetVector(), Version: a.GetVersion(), AdminUser: a.GetAdminUser(),
		CredentialsFile: a.GetCredentialsFile(), Connection: a.GetConnection()}
}

// registerDBAppTools adds the database application tools.
func (s *Server) registerDBAppTools(srv *mcp.Server) {
	addRead(s, srv, readOnlyTool("haify_app_list", "List database apps",
		"List the apps (haify app): single-instance PostgreSQL, MySQL/MariaDB, Redis or RustFS (S3) "+
			"on a resource's DRBD volume, failed over by drbd-reactor. Shows engine, resource, service IP and port."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, dbAppListOut, error) {
			s.logger.Debug("haify_app_list: listing apps")
			apps, err := s.client.ListApps(ctx)
			if err != nil {
				s.logger.Debug("haify_app_list: list failed", zap.Error(err))
				return nil, dbAppListOut{}, err
			}
			out := dbAppListOut{Apps: make([]dbAppOut, 0, len(apps))}
			for _, a := range apps {
				s.logger.Debug("haify_app_list: app", zap.String("name", a.GetName()), zap.String("engine", a.GetEngine()),
					zap.String("resource", a.GetResource()), zap.String("service_ip", a.GetServiceIp()), zap.Uint32("port", a.GetPort()))
				out.Apps = append(out.Apps, dbAppFrom(a))
			}
			s.logger.Debug("haify_app_list: listed", zap.Int("count", len(out.Apps)))
			return nil, out, nil
		})

	addRead(s, srv, readOnlyTool("haify_app_status", "Database app status",
		"Where a database app runs (the node its resource is Primary on), whether its unit is active "+
			"there and whether the service answers its health probe (pg_isready, mysqladmin ping, redis PING, "+
			"rustfs /health/ready)."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in dbAppNameIn) (*mcp.CallToolResult, dbAppStatusOut, error) {
			s.logger.Debug("haify_app_status: reading app status", zap.String("name", in.Name))
			st, err := s.client.GetAppStatus(ctx, in.Name)
			if err != nil {
				s.logger.Debug("haify_app_status: read failed", zap.String("name", in.Name), zap.Error(err))
				return nil, dbAppStatusOut{}, err
			}
			s.logger.Debug("haify_app_status: read", zap.String("name", in.Name), zap.String("state", st.State),
				zap.String("primary", st.PrimaryNode), zap.String("service_state", st.ServiceState),
				zap.Bool("healthy", st.Healthy), zap.Strings("nodes", st.Nodes))
			return nil, dbAppStatusOut{dbAppOut: dbAppFrom(st.App), State: st.State, PrimaryNode: st.PrimaryNode,
				ServiceState: st.ServiceState, Healthy: st.Healthy, Nodes: st.Nodes}, nil
		})

	addWrite(s, srv, writeTool("haify_app_create", "Create a database app",
		"Run PostgreSQL (optionally with pgvector), MySQL/MariaDB, Redis or RustFS (an S3-compatible object "+
			"store, single-node: its redundancy is the DRBD replicas) on an existing resource with at "+
			"least two diskful replicas. Checks every replica first (engine installed in the same version and "+
			"place, same daemon uid/gid, OCF agents, port free) and refuses with the reason; then formats the "+
			"volume if blank, initializes the database once, and hands it to drbd-reactor: mount, database, "+
			"service IP last. Failover keeps every acknowledged write (DRBD protocol C); clients reconnect to "+
			"the service IP. The generated password (for rustfs: the secret key of access key admin) is NOT "+
			"returned here: it is kept root-only on the volume (credentials_file), and the CLI's `haify app "+
			"create` prints it once."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in dbAppCreateIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_app_create: creating app", zap.String("name", in.Name), zap.String("engine", in.Engine),
				zap.String("resource", in.Resource), zap.String("service_ip", in.ServiceIP), zap.Uint32("port", in.Port),
				zap.Bool("vector", in.Vector))
			resp, err := s.client.CreateApp(ctx, client.AppCreateRequest{Name: in.Name, Engine: in.Engine,
				Resource: in.Resource, ServiceIP: in.ServiceIP, Port: in.Port, Vector: in.Vector})
			if err != nil {
				s.logger.Debug("haify_app_create: create failed", zap.String("name", in.Name), zap.String("resource", in.Resource),
					zap.Error(err))
				return nil, opResult{}, err
			}
			// The response carries the generated password, which is never traced.
			s.logger.Debug("haify_app_create: created", zap.String("name", resp.App.GetName()),
				zap.Bool("data_reused", resp.DataReused), zap.String("credentials_file", resp.App.GetCredentialsFile()))
			a := resp.App
			detail := fmt.Sprintf("app %s (%s) created on resource %s; connect with %s as %s; the password is "+
				"in %s (root, on the node running it)", a.GetName(), a.GetEngine(), a.GetResource(),
				a.GetConnection(), a.GetAdminUser(), a.GetCredentialsFile())
			if resp.DataReused {
				s.logger.Debug("haify_app_create: resource already held the app's data, reporting it kept", zap.String("name", in.Name))
				detail = fmt.Sprintf("app %s (%s) created on resource %s, which already held its data; the data "+
					"and its credentials (%s) are kept", a.GetName(), a.GetEngine(), a.GetResource(), a.GetCredentialsFile())
			}
			return nil, ok(detail), nil
		})

	addWrite(s, srv, writeTool("haify_app_snapshot", "Snapshot a database app",
		"Take a snapshot of every volume of a database app's resource on every replica, with the database "+
			"flushed and frozen on its node for the seconds it takes (the node thaws it by itself after 60s "+
			"if the controller does not)."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in dbAppSnapshotIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_app_snapshot: snapshotting app", zap.String("name", in.Name), zap.String("snapshot", in.Snapshot))
			resp, err := s.client.SnapshotApp(ctx, in.Name, in.Snapshot)
			if err != nil {
				s.logger.Debug("haify_app_snapshot: snapshot failed", zap.String("name", in.Name),
					zap.String("snapshot", in.Snapshot), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_app_snapshot: done", zap.String("name", in.Name), zap.String("message", resp.Message))
			return nil, ok(resp.Message), nil
		})

	addWrite(s, srv, destructiveTool("haify_app_failover", "Fail a database app over",
		"Planned switchover: the node running the app stops it (service IP, database, mount) and another "+
			"replica starts it. Clients are disconnected for the seconds that takes and reconnect to the "+
			"service IP. Fails when no other replica took over."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in dbAppNameIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_app_failover: failing app over", zap.String("name", in.Name))
			resp, err := s.client.FailoverApp(ctx, in.Name)
			if err != nil {
				s.logger.Debug("haify_app_failover: failover failed", zap.String("name", in.Name), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_app_failover: done", zap.String("name", in.Name), zap.String("message", resp.Message))
			return nil, ok(resp.Message), nil
		})

	addWrite(s, srv, destructiveTool("haify_app_delete", "Delete a database app",
		"Stop a database app and remove it from drbd-reactor and every node. The resource and its data are "+
			"kept (creating the app again picks them up) unless delete_data is set, which deletes the resource."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in dbAppDeleteIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_app_delete: deleting app", zap.String("name", in.Name), zap.Bool("delete_data", in.DeleteData))
			msg, err := s.client.DeleteApp(ctx, in.Name, in.DeleteData)
			if err != nil {
				s.logger.Debug("haify_app_delete: delete failed", zap.String("name", in.Name), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_app_delete: deleted", zap.String("name", in.Name), zap.String("message", msg))
			return nil, ok(msg), nil
		})
}
