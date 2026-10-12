package mcpserver

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/k8sapp"
)

// The Kubernetes (CSI) tools are a separate MCP server, haify-k8s, from the
// bare-metal ones: they talk to a Kubernetes API server rather than the Haify
// controller, need a kubeconfig the bare-metal server has no use for, and act
// on Kubernetes objects. Keeping them apart lets a client mount either one, and
// every tool here is named haify_k8s_* so the model can tell which side a call
// lands on.

// AppManager creates, lists and deletes databases on Kubernetes backed by Haify
// volumes. *k8sapp.Manager implements it.
type AppManager interface {
	Create(ctx context.Context, r k8sapp.Request) (*k8sapp.Created, error)
	List(ctx context.Context) ([]k8sapp.Status, error)
	Delete(ctx context.Context, r k8sapp.DeleteRequest) (*k8sapp.Deleted, error)
}

type appCreateIn struct {
	Template     string `json:"template" jsonschema:"application to run: mysql or postgres"`
	Name         string `json:"name,omitempty" jsonschema:"name of the Deployment, Service and claim (default: the template name)"`
	Namespace    string `json:"namespace,omitempty" jsonschema:"Kubernetes namespace, created if missing (default: default)"`
	Size         string `json:"size,omitempty" jsonschema:"volume size as a Kubernetes quantity, e.g. 10Gi (default 5Gi)"`
	StorageClass string `json:"storage_class,omitempty" jsonschema:"Haify StorageClass (default: one that keeps data on the database's node)"`
	Image        string `json:"image,omitempty" jsonschema:"container image override (default mysql:8.4 or postgres:17)"`
}

type appDeleteIn struct {
	Name       string `json:"name" jsonschema:"name of the app (its Deployment)"`
	Namespace  string `json:"namespace,omitempty" jsonschema:"Kubernetes namespace (default: default)"`
	DeleteData bool   `json:"delete_data,omitempty" jsonschema:"also delete the volume claim holding the data and the secret holding its password"`
}

type appListOut struct {
	Apps      []k8sapp.Status `json:"apps"`
	Templates []string        `json:"templates" jsonschema:"applications haify_k8s_app_create can run"`
}

// NewK8s builds the haify-k8s server. AllowWrite and ReadOnly mean the same as
// on the bare-metal server.
func NewK8s(apps AppManager, logger *zap.Logger, opts Options) *Server {
	logger.Debug("NewK8s: building haify-k8s server", zap.Bool("has_apps", apps != nil))
	s := New(nil, logger, opts)
	s.apps, s.k8s = apps, true
	return s
}

func (s *Server) k8sMCPServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "haify-k8s",
		Title:   "Haify on Kubernetes (CSI)",
		Version: s.version,
	}, nil)
	s.logger.Debug("Server.k8sMCPServer: registering Kubernetes app tools")
	s.registerAppTools(srv)
	return srv
}

// registerAppTools adds the Kubernetes application tools.
func (s *Server) registerAppTools(srv *mcp.Server) {
	addRead(s, srv, readOnlyTool("haify_k8s_app_list", "List Haify-backed apps",
		"List databases on Kubernetes created by haify_k8s_app_create: namespace, name, template, "+
			"whether it is ready, the node it runs on, its Service address and the DRBD resource "+
			"holding its data. Also lists the templates available."),
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, appListOut, error) {
			s.logger.Debug("haify_k8s_app_list: listing apps")
			apps, err := s.apps.List(ctx)
			if err != nil {
				s.logger.Debug("haify_k8s_app_list: list failed", zap.Error(err))
				return nil, appListOut{}, err
			}
			for _, a := range apps {
				s.logger.Debug("haify_k8s_app_list: app", zap.String("namespace", a.Namespace), zap.String("name", a.Name),
					zap.String("template", a.Template), zap.Bool("ready", a.Ready))
			}
			templates := k8sapp.Templates()
			s.logger.Debug("haify_k8s_app_list: listed", zap.Int("count", len(apps)), zap.Strings("templates", templates))
			return nil, appListOut{Apps: apps, Templates: templates}, nil
		})

	addWrite(s, srv, writeTool("haify_k8s_app_create", "Create an HA database on Kubernetes",
		"Run MySQL or PostgreSQL on Kubernetes with its data on a Haify volume: DRBD keeps a "+
			"replica on two nodes and a tiebreaker on a third. One database pod; if its node fails, "+
			"Kubernetes restarts it on the other replica node after about 30 seconds plus the "+
			"database's start-up, with every committed write intact. It is storage failover, not "+
			"database replication. Creates a Secret <name>-auth (generated password), a "+
			"PersistentVolumeClaim <name>-data, a Deployment and a Service <name>. Never overwrites: "+
			"an app that already exists is an error."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in appCreateIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_k8s_app_create: creating app", zap.String("template", in.Template),
				zap.String("name", in.Name), zap.String("namespace", in.Namespace), zap.String("size", in.Size),
				zap.String("storage_class", in.StorageClass), zap.String("image", in.Image))
			created, err := s.apps.Create(ctx, k8sapp.Request{
				Template:     in.Template,
				Name:         in.Name,
				Namespace:    in.Namespace,
				Size:         in.Size,
				StorageClass: strings.TrimSpace(in.StorageClass),
				Image:        in.Image,
			})
			if err != nil {
				s.logger.Debug("haify_k8s_app_create: create failed", zap.String("template", in.Template),
					zap.String("name", in.Name), zap.String("namespace", in.Namespace), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_k8s_app_create: created", zap.String("name", in.Name), zap.String("namespace", in.Namespace))
			return nil, ok(created.Message), nil
		})

	addWrite(s, srv, destructiveTool("haify_k8s_app_delete", "Delete a Haify-backed app",
		"Delete a database created by haify_k8s_app_create: its Deployment and Service. The volume claim "+
			"<name>-data and the secret <name>-auth are kept, so creating the app again with the same name and "+
			"template runs it on the same data with the same password, unless delete_data is set, which deletes "+
			"them too (and with them the data, as the StorageClass's reclaim policy decides). Only objects "+
			"haify_k8s_app_create made are deleted; anything else of the same name is left alone."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in appDeleteIn) (*mcp.CallToolResult, opResult, error) {
			s.logger.Debug("haify_k8s_app_delete: deleting app", zap.String("name", in.Name),
				zap.String("namespace", in.Namespace), zap.Bool("delete_data", in.DeleteData))
			deleted, err := s.apps.Delete(ctx, k8sapp.DeleteRequest{Name: in.Name, Namespace: in.Namespace,
				DeleteData: in.DeleteData})
			if err != nil {
				s.logger.Debug("haify_k8s_app_delete: delete failed", zap.String("name", in.Name),
					zap.String("namespace", in.Namespace), zap.Error(err))
				return nil, opResult{}, err
			}
			s.logger.Debug("haify_k8s_app_delete: deleted", zap.String("name", in.Name), zap.String("namespace", in.Namespace))
			return nil, ok(deleted.Message), nil
		})
}
