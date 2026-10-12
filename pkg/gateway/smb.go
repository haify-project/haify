package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"

	v1 "github.com/haify-project/haify/api/proto/v1"
	"go.uber.org/zap"
)

// SMB gateway, workgroup edition.
//
// One Samba server per gateway, standalone (no domain), with its users in a
// passdb kept on the gateway's own state volume. Everything that has to move
// with the gateway lives there — smb.conf, the share definitions, the passdb
// and secrets, Samba's lock and state directories — so a failover brings up the
// same server with the same users and shares on the next node. What does not
// move is the open sessions: Samba without CTDB has no transparent failover, so
// clients reconnect, and a file a client held open across the switch sees the
// error a server restart would give it.
//
// Each gateway runs its own smbd, haify-smbd@<resource>.service, bound to the
// gateway's service IP only (`bind interfaces only`), so several SMB gateways
// can run on one node. That is also why the chain brings the service IP up
// BEFORE smbd, the opposite of the NFS and iSCSI chains: smbd binds to the
// addresses that exist when it starts. The cost is a moment, at either end of
// a switchover, in which the address refuses connections rather than ignoring
// them; SMB clients reconnect either way.
//
// Files are owned by one account, haify-smb, on every share (force user/group):
// access is decided per share, by `valid users` and `read only`, not by Unix
// permissions per user. That keeps file ownership meaningful after a failover,
// where per-user ownership would need every user's uid to match on every node.
// haify-smb, and the account behind every SMB user (passdb requires one), are
// created with the same uid on every node; the uids are recorded on the state
// volume and the unit creates any missing account before smbd starts.

const (
	smbPort = 445
	// smbOwner owns every file on every SMB share.
	smbOwner = "haify-smb"
	// smbUnitPath is the per-gateway smbd unit, installed on every node that
	// may run an SMB gateway.
	smbUnitPath = "/etc/systemd/system/haify-smbd@.service"
	// smbUsersHelper creates the accounts recorded on a gateway's state
	// volume; the unit runs it before smbd.
	smbUsersHelper = "/usr/local/libexec/haify-smb-users"
	// smbFirstUID is where allocated uids start, clear of distribution
	// ranges for regular users.
	smbFirstUID = 61000
)

// smbDir is where a gateway's Samba state lives: on its state volume.
func smbDir(resource string) string {
	return filepath.Join(DefaultClusterPrivateMountPath, resource, "smb")
}

// smbConfPath is the gateway's smb.conf.
func smbConfPath(resource string) string { return filepath.Join(smbDir(resource), "smb.conf") }

// smbSharesPath is the include file holding the gateway's share sections.
func smbSharesPath(resource string) string { return filepath.Join(smbDir(resource), "shares.conf") }

// smbUsersPath records "<name>:<uid>" for every account the gateway uses.
func smbUsersPath(resource string) string { return filepath.Join(smbDir(resource), "users") }

// smbShareRoot is where the gateway's data volume is mounted; share paths are
// directories under it.
func smbShareRoot(resource string) string { return filepath.Join(DefaultExportBasePath, resource) }

// smbUnit is the systemd unit that runs one gateway's smbd.
func smbUnit(resource string) string { return fmt.Sprintf("haify-smbd@%s.service", resource) }

// smbUnitContent runs smbd from the gateway's own config. %i is the resource.
const smbUnitContent = `[Unit]
Description=Haify SMB gateway %i
After=network.target

[Service]
Type=simple
ExecStartPre=` + smbUsersHelper + ` ` + DefaultClusterPrivateMountPath + `/%i/smb/users
ExecStart=/usr/sbin/smbd --foreground --no-process-group -s ` + DefaultClusterPrivateMountPath + `/%i/smb/smb.conf
ExecReload=/bin/kill -HUP $MAINPID
LimitNOFILE=16384
Restart=no
`

// smbUsersHelperContent creates the accounts listed in a users file, each
// with its recorded uid and a group of the same id, and refuses to start the
// gateway when a name or id is taken differently on this node: a share served
// with the wrong owner is worse than one that is down and says why.
const smbUsersHelperContent = `#!/bin/sh
f=$1
[ -f "$f" ] || exit 0
while IFS=: read -r name uid; do
  [ -n "$name" ] && [ -n "$uid" ] || continue
  cur=$(id -u "$name" 2>/dev/null)
  if [ -z "$cur" ]; then
    if getent passwd "$uid" >/dev/null; then
      echo "SMB gateway: uid $uid for $name is taken on $(hostname)" >&2; exit 1
    fi
    getent group "$name" >/dev/null || groupadd -g "$uid" "$name" || exit 1
    useradd -M -N -g "$name" -u "$uid" -d /nonexistent -s /usr/sbin/nologin "$name" || exit 1
  elif [ "$cur" != "$uid" ]; then
    echo "SMB gateway: $name has uid $cur on $(hostname) but $uid on the gateway" >&2; exit 1
  fi
done < "$f"
`

// smbPrereqs: Samba's server and tools, and no distribution smbd holding port
// 445 on every address, which would keep the gateway's smbd from binding.
func smbPrereqs() gatewayPrereqs {
	return gatewayPrereqs{
		agents:  []string{"Filesystem", "IPaddr2"},
		tools:   []string{"smbd", "smbpasswd", "smbcontrol", "pdbedit"},
		install: "samba (Debian/Ubuntu and EL)",
		checks: []string{
			`! systemctl is-active -q smbd 2>/dev/null || missing="$missing (the distribution smbd is running and holds port 445: systemctl disable --now smbd nmbd)"`,
		},
	}
}

// prepareSMBNode installs the smbd unit and the account helper.
func (m *Manager) prepareSMBNode(ctx context.Context, hosts []string) error {
	if len(hosts) == 0 {
		return nil
	}
	script := fmt.Sprintf(`set -e
mkdir -p %[1]s
echo %[2]s | base64 -d > %[3]s.new && chmod 755 %[3]s.new && mv %[3]s.new %[3]s
want=$(echo %[4]s | base64 -d)
if [ "$(cat %[5]s 2>/dev/null)" != "$want" ]; then
  printf '%%s\n' "$want" > %[5]s
  systemctl daemon-reload
fi`, filepath.Dir(smbUsersHelper), base64Of(smbUsersHelperContent), smbUsersHelper,
		base64Of(smbUnitContent), smbUnitPath)
	if err := m.runScript(ctx, hosts, script); err != nil {
		return fmt.Errorf("install the SMB gateway unit: %w", err)
	}
	return nil
}

// SMBManager handles SMB gateways.
type SMBManager struct {
	*Manager
}

// NewSMBManager creates an SMB gateway manager.
func NewSMBManager(m *Manager) *SMBManager {
	return &SMBManager{Manager: m}
}

// CreateSMBGateway creates an SMB gateway on a resource: the Samba state on the
// state volume, the share root on the data volume, and the promoter.
func (s *SMBManager) CreateSMBGateway(ctx context.Context, req *v1.CreateSMBGatewayRequest) (*v1.CreateSMBGatewayResponse, error) {
	fail := func(err error) (*v1.CreateSMBGatewayResponse, error) {
		return &v1.CreateSMBGatewayResponse{Success: false, Message: err.Error()}, err
	}
	s.logger.Info("Creating SMB gateway",
		zap.String("resource", req.Resource), zap.String("service_ip", req.ServiceIp))

	serviceIP, err := parseServiceIP(req.ServiceIp)
	if err != nil {
		return fail(invalidArgument(fmt.Errorf("invalid service IP: %w", err)))
	}
	workgroup, err := validateSMBWorkgroup(req.Workgroup)
	if err != nil {
		return fail(invalidArgument(err))
	}
	share := SMBShare{Name: req.ShareName, ReadOnly: req.ReadOnly, ValidUsers: req.ValidUsers}
	if share.Name == "" {
		share.Name = req.Resource
	}
	if err := share.validate(); err != nil {
		return fail(invalidArgument(err))
	}

	if res, rerr := s.resources.GetResource(ctx, req.Resource); rerr == nil && res != nil {
		if err := s.checkGatewayPrereqs(ctx, gatewayNodes(res), smbPrereqs()); err != nil {
			return fail(err)
		}
	}
	if err := s.resources.EnsureGatewayVolumes(ctx, req.Resource, 2); err != nil {
		return fail(fmt.Errorf("failed to provision gateway state volume: %w", err))
	}
	resInfo, err := s.resources.GetResource(ctx, req.Resource)
	if err != nil {
		return fail(fmt.Errorf("failed to get resource info: %w", err))
	}
	if len(resInfo.Volumes) < 2 {
		return fail(fmt.Errorf("SMB gateway requires at least 2 volumes (got %d): one for Samba's state, one for the shares", len(resInfo.Volumes)))
	}
	drbdDevice, err := s.getDRBDDevice(ctx, req.Resource)
	if err != nil {
		drbdDevice = "/dev/drbd0"
	}
	stateDev, payload := clusterPrivateAndPayload(resInfo.Volumes, drbdDevice)
	dataDev := payloadDevice(payload, drbdDevice)
	if err := s.ensureGatewayPrerequisites(ctx, req.Resource, resInfo.Nodes, stateDev, dataDev); err != nil {
		return fail(err)
	}

	run := gatewayNodes(resInfo)
	if err := s.prepareSMBNode(ctx, run); err != nil {
		return fail(err)
	}
	ownerUID, err := s.allocateUID(ctx, run, smbOwner, nil)
	if err != nil {
		return fail(err)
	}
	// Seed the state volume on the node ensureGatewayPrerequisites promoted.
	seed := smbSeedScript(req.Resource, stateDev, dataDev, smbGlobalConfig(req.Resource, workgroup, serviceIP),
		share, ownerUID)
	if err := s.runScript(ctx, resInfo.Nodes[:1], seed); err != nil {
		return fail(fmt.Errorf("prepare Samba's state on the gateway volume: %w", err))
	}

	config, err := generateSMBGatewayConfig(req.Resource, stateDev, dataDev, serviceIP)
	if err != nil {
		return fail(err)
	}
	pluginID := "haify-smb-" + req.Resource
	if err := s.writeReactorConfig(ctx, req.Resource, pluginID, config); err != nil {
		return fail(fmt.Errorf("failed to write config: %w", err))
	}
	return &v1.CreateSMBGatewayResponse{
		Success:    true,
		Message:    fmt.Sprintf("SMB gateway created: \\\\%s\\%s (add users with `haify gateway smb user set`)", serviceIP.IP, share.Name),
		ConfigPath: gatewayConfigPath(pluginID),
	}, nil
}

// generateSMBGatewayConfig is the promoter: state volume, data volume, service
// IP, then smbd (see the top of the file for why the IP comes before smbd).
func generateSMBGatewayConfig(resource, stateDev, dataDev string, ip *ServiceIP) (string, error) {
	tmpl := `# Haify SMB Gateway Configuration
# Generated by Haify Controller
# Resource: {{ .Resource }}

[[promoter]]

  [promoter.resources]

    [promoter.resources.{{ .Resource }}]
      on-drbd-demote-failure = "reboot-immediate"
      runner = "systemd"
      stop-services-on-exit = true
      target-as = "BindsTo"

      start = [
        "ocf:heartbeat:Filesystem fs_cluster_private device={{ .StateDev }} directory={{ .StatePath }} fstype=ext4 run_fsck=no force_unmount=false",
        "ocf:heartbeat:Filesystem fs_share device={{ .DataDev }} directory={{ .ShareRoot }} fstype=ext4 run_fsck=no force_unmount=false",
        "ocf:heartbeat:IPaddr2 service_ip ip={{ .IP }} cidr_netmask={{ .Prefix }}",
        "{{ .Unit }}",
      ]
`
	return executeTemplate(tmpl, map[string]any{
		"Resource":  resource,
		"StateDev":  stateDev,
		"StatePath": filepath.Join(DefaultClusterPrivateMountPath, resource),
		"DataDev":   dataDev,
		"ShareRoot": smbShareRoot(resource),
		"IP":        ip.IP.String(),
		"Prefix":    ip.Prefix,
		"Unit":      smbUnit(resource),
	})
}

// smbGlobalConfig is the gateway's smb.conf: a standalone server bound to the
// service IP, with every piece of state under the state volume.
func smbGlobalConfig(resource, workgroup string, ip *ServiceIP) string {
	d := smbDir(resource)
	lines := []string{
		"# Haify SMB gateway " + resource + " (generated; shares are in shares.conf)",
		"[global]",
		"  workgroup = " + workgroup,
		"  netbios name = " + smbNetbiosName(resource),
		"  server role = standalone server",
		fmt.Sprintf("  interfaces = %s/%d", ip.IP, ip.Prefix),
		"  bind interfaces only = yes",
		fmt.Sprintf("  smb ports = %d", smbPort),
		"  disable netbios = yes",
		"  server min protocol = SMB2_10",
		"  map to guest = never",
		"  passdb backend = tdbsam:" + d + "/private/passdb.tdb",
		"  private dir = " + d + "/private",
		"  lock directory = " + d + "/lock",
		"  state directory = " + d + "/state",
		"  cache directory = " + d + "/cache",
		"  pid directory = " + d + "/run",
		"  ncalrpc dir = " + d + "/run/ncalrpc",
		"  log file = /var/log/samba/haify-" + resource + ".log",
		"  max log size = 10000",
		"  load printers = no",
		"  printing = bsd",
		"  printcap name = /dev/null",
		"  disable spoolss = yes",
		"  include = " + smbSharesPath(resource),
	}
	return strings.Join(lines, "\n") + "\n"
}

// smbNetbiosName is the server's NetBIOS name: at most 15 characters.
func smbNetbiosName(resource string) string {
	n := strings.ToUpper(strings.NewReplacer("_", "-", ".", "-").Replace(resource))
	if len(n) > 15 {
		n = n[:15]
	}
	return strings.Trim(n, "-")
}

// smbSeedScript lays out Samba's state on the state volume, mounted for the
// purpose on the node that holds the resource Primary. smb.conf is rewritten
// (the service IP may have changed); shares, users and the passdb of a
// gateway created on this resource before are kept.
func smbSeedScript(resource, stateDev, dataDev, globalConf string, share SMBShare, ownerUID int) string {
	d := "$st/smb"
	return fmt.Sprintf(`set -e
st=$(mktemp -d) dt=$(mktemp -d)
trap 'umount "$dt" 2>/dev/null; umount "$st" 2>/dev/null; rmdir "$st" "$dt"' EXIT
mount %[1]s "$st"
mount %[2]s "$dt"
mkdir -p %[3]s/private %[3]s/lock %[3]s/state %[3]s/cache %[3]s/run
chmod 700 %[3]s/private
echo %[4]s | base64 -d > %[3]s/smb.conf
grep -q '^%[5]s:' %[3]s/users 2>/dev/null || echo '%[5]s:%[6]d' >> %[3]s/users
%[7]s %[3]s/users
if [ ! -f %[3]s/shares.conf ]; then
  echo %[8]s | base64 -d > %[3]s/shares.conf
  mkdir -p "$dt/%[9]s"
  chown %[5]s:%[5]s "$dt/%[9]s"
fi
mkdir -p /var/log/samba
`, stateDev, dataDev, d, base64Of(globalConf), smbOwner, ownerUID, smbUsersHelper,
		base64Of(share.section(resource)), share.Path)
}

func base64Of(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
