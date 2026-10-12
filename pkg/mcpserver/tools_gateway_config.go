package mcpserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// registerGatewayConfigReads adds read-only views of each gateway's exports,
// LUNs, namespaces and allow-lists. The add/remove tools below also answer
// "list" for clients that already call them that way, but they are
// destructive and so absent for read and operate tokens; these are not.
func (s *Server) registerGatewayConfigReads(srv *mcp.Server) {
	addRead(s, srv, readOnlyTool("haify_nfs_export_list", "List NFS exports",
		"List the exports of an NFS gateway: directory, fsid, client spec and options."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in gatewayResourceIn) (*mcp.CallToolResult, nfsExportsOut, error) {
			out, err := s.nfsExportList(ctx, in.Resource)
			return nil, out, err
		})
	addRead(s, srv, readOnlyTool("haify_iscsi_lun_list", "List iSCSI LUNs",
		"List the LUNs of an iSCSI gateway and the device behind each."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in gatewayResourceIn) (*mcp.CallToolResult, iscsiLunsOut, error) {
			out, err := s.iscsiLunList(ctx, in.Resource)
			return nil, out, err
		})
	addRead(s, srv, readOnlyTool("haify_iscsi_initiator_list", "List iSCSI initiators",
		"List the initiator IQNs on an iSCSI gateway's allow-list."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in gatewayResourceIn) (*mcp.CallToolResult, stringListOut, error) {
			out, err := s.iscsiInitiatorList(ctx, in.Resource)
			return nil, out, err
		})
	addRead(s, srv, readOnlyTool("haify_iscsi_chap_get", "Show iSCSI CHAP settings",
		"Whether an iSCSI gateway requires one-way CHAP, and its username. The password is never returned."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in gatewayResourceIn) (*mcp.CallToolResult, iscsiChapOut, error) {
			out, err := s.iscsiChapGet(ctx, in.Resource)
			return nil, out, err
		})
	addRead(s, srv, readOnlyTool("haify_nvme_namespace_list", "List NVMe namespaces",
		"List the namespaces of an NVMe-oF gateway: ID, backing device, UUID and NQN."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in gatewayResourceIn) (*mcp.CallToolResult, nvmeNamespacesOut, error) {
			out, err := s.nvmeNamespaceList(ctx, in.Resource)
			return nil, out, err
		})
	addRead(s, srv, readOnlyTool("haify_nvme_host_list", "List NVMe hosts",
		"List the host NQNs on an NVMe-oF gateway's allow-list."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in gatewayResourceIn) (*mcp.CallToolResult, stringListOut, error) {
			out, err := s.nvmeHostList(ctx, in.Resource)
			return nil, out, err
		})
}

func (s *Server) nfsExportList(ctx context.Context, resource string) (nfsExportsOut, error) {
	s.logger.Debug("Server.nfsExportList: listing exports", zap.String("resource", resource))
	exports, err := s.client.ListNFSExports(ctx, resource)
	if err != nil {
		s.logger.Debug("Server.nfsExportList: list failed", zap.String("resource", resource), zap.Error(err))
		return nfsExportsOut{}, err
	}
	out := nfsExportsOut{Exports: make([]nfsExportOut, 0, len(exports))}
	for _, e := range exports {
		s.logger.Debug("Server.nfsExportList: export", zap.String("resource", resource), zap.String("directory", e.Directory),
			zap.String("fsid", e.Fsid), zap.String("client_spec", e.Clientspec), zap.String("options", e.Options))
		out.Exports = append(out.Exports, nfsExportOut{
			Directory:  e.Directory,
			Fsid:       e.Fsid,
			ClientSpec: e.Clientspec,
			Options:    e.Options,
		})
	}
	s.logger.Debug("Server.nfsExportList: listed", zap.String("resource", resource), zap.Int("count", len(out.Exports)))
	return out, nil
}

func (s *Server) iscsiLunList(ctx context.Context, resource string) (iscsiLunsOut, error) {
	s.logger.Debug("Server.iscsiLunList: listing LUNs", zap.String("resource", resource))
	luns, err := s.client.ListISCSILUNs(ctx, resource)
	if err != nil {
		s.logger.Debug("Server.iscsiLunList: list failed", zap.String("resource", resource), zap.Error(err))
		return iscsiLunsOut{}, err
	}
	out := iscsiLunsOut{Luns: make([]iscsiLunOut, 0, len(luns))}
	for _, l := range luns {
		s.logger.Debug("Server.iscsiLunList: LUN", zap.String("resource", resource), zap.Int32("lun", l.Lun),
			zap.String("device", l.Device), zap.String("iqn", l.TargetIqn))
		out.Luns = append(out.Luns, iscsiLunOut{Lun: l.Lun, Device: l.Device, TargetIQN: l.TargetIqn})
	}
	s.logger.Debug("Server.iscsiLunList: listed", zap.String("resource", resource), zap.Int("count", len(out.Luns)))
	return out, nil
}

func (s *Server) iscsiInitiatorList(ctx context.Context, resource string) (stringListOut, error) {
	s.logger.Debug("Server.iscsiInitiatorList: listing initiators", zap.String("resource", resource))
	initiators, err := s.client.ListISCSIInitiators(ctx, resource)
	if err != nil {
		s.logger.Debug("Server.iscsiInitiatorList: list failed", zap.String("resource", resource), zap.Error(err))
		return stringListOut{}, err
	}
	s.logger.Debug("Server.iscsiInitiatorList: listed", zap.String("resource", resource), zap.Strings("initiators", initiators))
	return stringListOut{Items: initiators}, nil
}

func (s *Server) iscsiChapGet(ctx context.Context, resource string) (iscsiChapOut, error) {
	s.logger.Debug("Server.iscsiChapGet: reading CHAP settings", zap.String("resource", resource))
	chap, err := s.client.GetISCSIChap(ctx, resource)
	if err != nil {
		s.logger.Debug("Server.iscsiChapGet: read failed", zap.String("resource", resource), zap.Error(err))
		return iscsiChapOut{}, err
	}
	s.logger.Debug("Server.iscsiChapGet: read", zap.String("resource", resource), zap.Bool("has_username", chap.Username != ""))
	return iscsiChapOut{Username: chap.Username}, nil
}

func (s *Server) nvmeNamespaceList(ctx context.Context, resource string) (nvmeNamespacesOut, error) {
	s.logger.Debug("Server.nvmeNamespaceList: listing namespaces", zap.String("resource", resource))
	namespaces, err := s.client.ListNVMeNamespaces(ctx, resource)
	if err != nil {
		s.logger.Debug("Server.nvmeNamespaceList: list failed", zap.String("resource", resource), zap.Error(err))
		return nvmeNamespacesOut{}, err
	}
	out := nvmeNamespacesOut{Namespaces: make([]nvmeNamespaceOut, 0, len(namespaces))}
	for _, ns := range namespaces {
		s.logger.Debug("Server.nvmeNamespaceList: namespace", zap.String("resource", resource),
			zap.Int32("namespace_id", ns.NamespaceId), zap.String("backing_path", ns.BackingPath), zap.String("nqn", ns.Nqn))
		out.Namespaces = append(out.Namespaces, nvmeNamespaceOut{
			NamespaceID: ns.NamespaceId,
			BackingPath: ns.BackingPath,
			UUID:        ns.Uuid,
			NQN:         ns.Nqn,
		})
	}
	s.logger.Debug("Server.nvmeNamespaceList: listed", zap.String("resource", resource), zap.Int("count", len(out.Namespaces)))
	return out, nil
}

func (s *Server) nvmeHostList(ctx context.Context, resource string) (stringListOut, error) {
	s.logger.Debug("Server.nvmeHostList: listing hosts", zap.String("resource", resource))
	hosts, err := s.client.ListNVMeHosts(ctx, resource)
	if err != nil {
		s.logger.Debug("Server.nvmeHostList: list failed", zap.String("resource", resource), zap.Error(err))
		return stringListOut{}, err
	}
	s.logger.Debug("Server.nvmeHostList: listed", zap.String("resource", resource), zap.Strings("hosts", hosts))
	return stringListOut{Items: hosts}, nil
}

func (s *Server) registerNFSConfig(srv *mcp.Server) {
	addWrite(s, srv, destructiveTool("haify_nfs_exports", "Manage NFS exports",
		"Add or remove NFS exports on an existing NFS gateway. Action list is still accepted; "+
			"haify_nfs_export_list does the same as a read-only tool."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in nfsExportsIn) (*mcp.CallToolResult, nfsExportsOut, error) {
			s.logger.Debug("haify_nfs_exports: managing", zap.String("action", in.Action), zap.String("resource", in.Resource), zap.String("export_path", in.ExportPath), zap.Int32("fsid", in.Fsid), zap.String("client_spec", in.ClientSpec), zap.String("options", in.Options))
			switch in.Action {
			case "list":
				out, err := s.nfsExportList(ctx, in.Resource)
				return nil, out, err
			case "add":
				if in.ExportPath == "" {
					s.logger.Debug("haify_nfs_exports: refusing, export_path missing for add")
					return nil, nfsExportsOut{}, fmt.Errorf("export_path is required for add")
				}
				if err := s.client.AddNFSExport(ctx, in.Resource, in.ExportPath, in.Fsid, in.ClientSpec, in.Options); err != nil {
					s.logger.Debug("haify_nfs_exports: add export failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, nfsExportsOut{}, err
				}
				s.logger.Debug("haify_nfs_exports: add export done", zap.String("resource", in.Resource))
				return nil, nfsExportsOut{Detail: fmt.Sprintf("export %s added to gateway %s", in.ExportPath, in.Resource)}, nil
			case "remove":
				if in.ExportPath == "" {
					s.logger.Debug("haify_nfs_exports: refusing, export_path missing for remove")
					return nil, nfsExportsOut{}, fmt.Errorf("export_path is required for remove")
				}
				if err := s.client.RemoveNFSExport(ctx, in.Resource, in.ExportPath); err != nil {
					s.logger.Debug("haify_nfs_exports: remove export failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, nfsExportsOut{}, err
				}
				s.logger.Debug("haify_nfs_exports: remove export done", zap.String("resource", in.Resource))
				return nil, nfsExportsOut{Detail: fmt.Sprintf("export %s removed from gateway %s", in.ExportPath, in.Resource)}, nil
			default:
				s.logger.Debug("haify_nfs_exports: unknown action", zap.String("action", in.Action))
				return nil, nfsExportsOut{}, badAction(in.Action, "list, add, or remove")
			}
		})
}

func (s *Server) registerISCSIConfig(srv *mcp.Server) {
	addWrite(s, srv, destructiveTool("haify_iscsi_luns", "Manage iSCSI LUNs",
		"Add or remove LUNs on an existing iSCSI gateway. Action list is still accepted; "+
			"haify_iscsi_lun_list does the same as a read-only tool."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in iscsiLunsIn) (*mcp.CallToolResult, iscsiLunsOut, error) {
			s.logger.Debug("haify_iscsi_luns: managing", zap.String("action", in.Action), zap.String("resource", in.Resource), zap.Int32("lun", in.Lun), zap.String("device", in.Device))
			switch in.Action {
			case "list":
				out, err := s.iscsiLunList(ctx, in.Resource)
				return nil, out, err
			case "add":
				if in.Device == "" {
					s.logger.Debug("haify_iscsi_luns: refusing, device missing for add")
					return nil, iscsiLunsOut{}, fmt.Errorf("device is required for add")
				}
				if err := s.client.AddISCSILUN(ctx, in.Resource, in.Lun, in.Device); err != nil {
					s.logger.Debug("haify_iscsi_luns: add LUN failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, iscsiLunsOut{}, err
				}
				s.logger.Debug("haify_iscsi_luns: add LUN done", zap.String("resource", in.Resource))
				return nil, iscsiLunsOut{Detail: fmt.Sprintf("LUN %d (%s) added to gateway %s", in.Lun, in.Device, in.Resource)}, nil
			case "remove":
				if err := s.client.RemoveISCSILUN(ctx, in.Resource, in.Lun); err != nil {
					s.logger.Debug("haify_iscsi_luns: remove LUN failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, iscsiLunsOut{}, err
				}
				s.logger.Debug("haify_iscsi_luns: remove LUN done", zap.String("resource", in.Resource))
				return nil, iscsiLunsOut{Detail: fmt.Sprintf("LUN %d removed from gateway %s", in.Lun, in.Resource)}, nil
			default:
				s.logger.Debug("haify_iscsi_luns: unknown action", zap.String("action", in.Action))
				return nil, iscsiLunsOut{}, badAction(in.Action, "list, add, or remove")
			}
		})

	addWrite(s, srv, destructiveTool("haify_iscsi_initiators", "Manage iSCSI initiators",
		"Add or remove initiator IQNs on an iSCSI gateway's allow-list. Action list is still accepted; "+
			"haify_iscsi_initiator_list does the same as a read-only tool."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in iscsiInitiatorsIn) (*mcp.CallToolResult, stringListOut, error) {
			s.logger.Debug("haify_iscsi_initiators: managing", zap.String("action", in.Action), zap.String("resource", in.Resource), zap.String("iqn", in.IQN))
			switch in.Action {
			case "list":
				out, err := s.iscsiInitiatorList(ctx, in.Resource)
				return nil, out, err
			case "add":
				if in.IQN == "" {
					s.logger.Debug("haify_iscsi_initiators: refusing, iqn missing for add")
					return nil, stringListOut{}, fmt.Errorf("iqn is required for add")
				}
				if err := s.client.AddISCSIInitiator(ctx, in.Resource, in.IQN); err != nil {
					s.logger.Debug("haify_iscsi_initiators: add initiator failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, stringListOut{}, err
				}
				s.logger.Debug("haify_iscsi_initiators: add initiator done", zap.String("resource", in.Resource))
				return nil, stringListOut{Detail: fmt.Sprintf("initiator %s allowed on gateway %s", in.IQN, in.Resource)}, nil
			case "remove":
				if in.IQN == "" {
					s.logger.Debug("haify_iscsi_initiators: refusing, iqn missing for remove")
					return nil, stringListOut{}, fmt.Errorf("iqn is required for remove")
				}
				if err := s.client.RemoveISCSIInitiator(ctx, in.Resource, in.IQN); err != nil {
					s.logger.Debug("haify_iscsi_initiators: remove initiator failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, stringListOut{}, err
				}
				s.logger.Debug("haify_iscsi_initiators: remove initiator done", zap.String("resource", in.Resource))
				return nil, stringListOut{Detail: fmt.Sprintf("initiator %s removed from gateway %s", in.IQN, in.Resource)}, nil
			default:
				s.logger.Debug("haify_iscsi_initiators: unknown action", zap.String("action", in.Action))
				return nil, stringListOut{}, badAction(in.Action, "list, add, or remove")
			}
		})

	addWrite(s, srv, writeTool("haify_iscsi_chap", "Manage iSCSI CHAP",
		"Set one-way CHAP credentials on an iSCSI gateway (mutual CHAP is not supported). Action get is still accepted; "+
			"haify_iscsi_chap_get does the same as a read-only tool. Passwords are never returned."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in iscsiChapIn) (*mcp.CallToolResult, iscsiChapOut, error) {
			s.logger.Debug("haify_iscsi_chap: managing", zap.String("action", in.Action), zap.String("resource", in.Resource), zap.Bool("has_username", in.Username != ""), zap.Bool("has_password", in.Password != ""))
			switch in.Action {
			case "get":
				out, err := s.iscsiChapGet(ctx, in.Resource)
				return nil, out, err
			case "set":
				if in.Username == "" || in.Password == "" {
					s.logger.Debug("haify_iscsi_chap: refusing, username and password missing for set")
					return nil, iscsiChapOut{}, fmt.Errorf("username and password are required for set")
				}
				if err := s.client.SetISCSIChap(ctx, in.Resource, in.Username, in.Password, false); err != nil {
					s.logger.Debug("haify_iscsi_chap: set CHAP failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, iscsiChapOut{}, err
				}
				s.logger.Debug("haify_iscsi_chap: set CHAP done", zap.String("resource", in.Resource))
				return nil, iscsiChapOut{Detail: fmt.Sprintf("CHAP credentials set on gateway %s", in.Resource)}, nil
			default:
				s.logger.Debug("haify_iscsi_chap: unknown action", zap.String("action", in.Action))
				return nil, iscsiChapOut{}, badAction(in.Action, "get or set")
			}
		})
}

func (s *Server) registerNVMeConfig(srv *mcp.Server) {
	addWrite(s, srv, destructiveTool("haify_nvme_namespaces", "Manage NVMe namespaces",
		"Add or remove namespaces on an existing NVMe-oF gateway. Action list is still accepted; "+
			"haify_nvme_namespace_list does the same as a read-only tool."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in nvmeNamespacesIn) (*mcp.CallToolResult, nvmeNamespacesOut, error) {
			s.logger.Debug("haify_nvme_namespaces: managing", zap.String("action", in.Action), zap.String("resource", in.Resource), zap.String("device", in.Device), zap.Int32("namespace_id", in.NamespaceID))
			switch in.Action {
			case "list":
				out, err := s.nvmeNamespaceList(ctx, in.Resource)
				return nil, out, err
			case "add":
				if in.Device == "" {
					s.logger.Debug("haify_nvme_namespaces: refusing, device missing for add")
					return nil, nvmeNamespacesOut{}, fmt.Errorf("device is required for add")
				}
				if err := s.client.AddNVMeNamespace(ctx, in.Resource, in.Device); err != nil {
					s.logger.Debug("haify_nvme_namespaces: add namespace failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, nvmeNamespacesOut{}, err
				}
				s.logger.Debug("haify_nvme_namespaces: add namespace done", zap.String("resource", in.Resource))
				return nil, nvmeNamespacesOut{Detail: fmt.Sprintf("namespace %s added to gateway %s", in.Device, in.Resource)}, nil
			case "remove":
				if err := s.client.RemoveNVMeNamespace(ctx, in.Resource, in.NamespaceID); err != nil {
					s.logger.Debug("haify_nvme_namespaces: remove namespace failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, nvmeNamespacesOut{}, err
				}
				s.logger.Debug("haify_nvme_namespaces: remove namespace done", zap.String("resource", in.Resource))
				return nil, nvmeNamespacesOut{Detail: fmt.Sprintf("namespace %d removed from gateway %s", in.NamespaceID, in.Resource)}, nil
			default:
				s.logger.Debug("haify_nvme_namespaces: unknown action", zap.String("action", in.Action))
				return nil, nvmeNamespacesOut{}, badAction(in.Action, "list, add, or remove")
			}
		})

	addWrite(s, srv, destructiveTool("haify_nvme_hosts", "Manage NVMe hosts",
		"Add or remove host NQNs on an NVMe-oF gateway's allow-list. Action list is still accepted; "+
			"haify_nvme_host_list does the same as a read-only tool."),
		func(ctx context.Context, _ *mcp.CallToolRequest, in nvmeHostsIn) (*mcp.CallToolResult, stringListOut, error) {
			s.logger.Debug("haify_nvme_hosts: managing", zap.String("action", in.Action), zap.String("resource", in.Resource), zap.String("host_nqn", in.HostNQN))
			switch in.Action {
			case "list":
				out, err := s.nvmeHostList(ctx, in.Resource)
				return nil, out, err
			case "add":
				if in.HostNQN == "" {
					s.logger.Debug("haify_nvme_hosts: refusing, host_nqn missing for add")
					return nil, stringListOut{}, fmt.Errorf("host_nqn is required for add")
				}
				if err := s.client.AddNVMeHost(ctx, in.Resource, in.HostNQN); err != nil {
					s.logger.Debug("haify_nvme_hosts: add host failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, stringListOut{}, err
				}
				s.logger.Debug("haify_nvme_hosts: add host done", zap.String("resource", in.Resource))
				return nil, stringListOut{Detail: fmt.Sprintf("host %s allowed on gateway %s", in.HostNQN, in.Resource)}, nil
			case "remove":
				if in.HostNQN == "" {
					s.logger.Debug("haify_nvme_hosts: refusing, host_nqn missing for remove")
					return nil, stringListOut{}, fmt.Errorf("host_nqn is required for remove")
				}
				if err := s.client.RemoveNVMeHost(ctx, in.Resource, in.HostNQN); err != nil {
					s.logger.Debug("haify_nvme_hosts: remove host failed", zap.String("resource", in.Resource), zap.Error(err))
					return nil, stringListOut{}, err
				}
				s.logger.Debug("haify_nvme_hosts: remove host done", zap.String("resource", in.Resource))
				return nil, stringListOut{Detail: fmt.Sprintf("host %s removed from gateway %s", in.HostNQN, in.Resource)}, nil
			default:
				s.logger.Debug("haify_nvme_hosts: unknown action", zap.String("action", in.Action))
				return nil, stringListOut{}, badAction(in.Action, "list, add, or remove")
			}
		})
}
