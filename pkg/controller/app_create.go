package controller

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/apptemplate"
	"github.com/haify-project/haify/pkg/database"
)

// AppCreated is what creating an app reports. Password is set only when this
// call generated it; it is not kept anywhere but on the volume.
type AppCreated struct {
	App      *database.App
	Password string
	Primary  string // the node it was initialized on
	Reused   bool   // the volume already held this app's data
}

// appMinReplicas is the fewest diskful replicas an app runs on: with one
// there is nothing to fail over to.
const appMinReplicas = 2

// Create makes an app on an existing resource:
//
//  1. refuse anything that would put a second promoter on the resource, and
//     a resource with fewer than two diskful replicas;
//  2. probe every diskful node (agents, engine, versions, uid/gid, port);
//  3. promote the resource on one of them, stage the password there, and run
//     the initialization script (mkfs if blank, initdb, smoke test, unmount);
//  4. demote it, record the app, and install the unit and the promoter on
//     every diskful node, after which drbd-reactor starts it.
//
// Nothing is written to any node before step 3, so a refusal leaves the
// cluster as it was.
func (am *AppManager) Create(ctx context.Context, spec apptemplate.Spec) (*AppCreated, error) {
	log := am.c.traceLogger()
	log.Debug("AppManager.Create: start", zap.String("app", spec.Name), zap.String("engine", string(spec.Engine)),
		zap.String("resource", spec.Resource), zap.String("service_ip", spec.ServiceIP), zap.Int("port", spec.Port),
		zap.Bool("vector", spec.Vector))
	if err := spec.Normalize(); err != nil {
		log.Debug("AppManager.Create: invalid spec", zap.String("app", spec.Name), zap.Error(err))
		return nil, err
	}
	if err := am.ready(); err != nil {
		return nil, err
	}
	appOps.Lock()
	defer appOps.Unlock()
	logger := am.c.logger.With(zap.String("app", spec.Name), zap.String("resource", spec.Resource))

	if err := am.checkCreatable(ctx, spec); err != nil {
		log.Debug("AppManager.Create: not creatable", zap.String("app", spec.Name), zap.Error(err))
		return nil, err
	}
	hosts, err := am.rm().failoverHosts(ctx, spec.Resource)
	log.Debug("AppManager.Create: failover hosts", zap.String("resource", spec.Resource), zap.Strings("hosts", hosts), zap.Error(err))
	if err != nil {
		return nil, err
	}
	if len(hosts) < appMinReplicas {
		log.Debug("AppManager.Create: too few diskful replicas", zap.Int("count", len(hosts)), zap.Int("min", appMinReplicas))
		return nil, fmt.Errorf("resource %s has %d diskful replica(s) a promoter can run on; an app needs at least %d "+
			"(add one with haify resource add-replica)", spec.Resource, len(hosts), appMinReplicas)
	}
	device, err := am.dataDevice(ctx, spec.Resource)
	if err != nil {
		log.Debug("AppManager.Create: no data device", zap.Error(err))
		return nil, err
	}
	if err := am.checkNoForeignPromoter(ctx, spec, hosts); err != nil {
		log.Debug("AppManager.Create: foreign promoter found", zap.Error(err))
		return nil, err
	}
	agreed, err := am.checkPrereqs(ctx, spec, hosts, nil)
	if err != nil {
		log.Debug("AppManager.Create: prerequisites not met", zap.Error(err))
		return nil, err
	}
	log.Debug("AppManager.Create: nodes agree", zap.String("device", device), zap.String("version", agreed.Version),
		zap.String("server", agreed.Binaries.Server), zap.Int("uid", agreed.UID), zap.Int("gid", agreed.GID))

	primary, promoted, err := am.firstPrimary(ctx, spec.Resource, hosts)
	if err != nil {
		log.Debug("AppManager.Create: no node to initialize on", zap.Error(err))
		return nil, err
	}
	log.Debug("AppManager.Create: initializing on", zap.String("primary", primary), zap.Bool("promoted", promoted))
	logger.Info("Initializing app", zap.String("engine", string(spec.Engine)), zap.String("node", am.nodeName(primary)))
	password, err := generatePassword()
	if err != nil {
		return nil, err
	}
	out, initErr := am.initialize(ctx, primary, spec, agreed.Binaries, device, password)
	log.Debug("AppManager.Create: initialization finished", zap.String("primary", primary), zap.Error(initErr))
	// The promoter can take a resource only when no node holds it: demote it
	// once the volume is set up — or when initialization failed and the
	// promotion was ours. A Primary found already in place and refused by the
	// init (mounted elsewhere, say) belongs to whoever made it so.
	if promoted || initErr == nil {
		log.Debug("AppManager.Create: demoting after initialization", zap.String("primary", primary),
			zap.Bool("promoted", promoted), zap.Bool("init_ok", initErr == nil))
		if err := am.rm().SetSecondary(ctx, spec.Resource, primary); err != nil {
			log.Debug("AppManager.Create: demote failed", zap.String("primary", primary), zap.Error(err))
			if initErr != nil {
				return nil, fmt.Errorf("%w (and demoting %s afterwards failed: %v)", initErr, spec.Resource, err)
			}
			return nil, fmt.Errorf("the app was initialized on %s, but demoting %s there failed, so drbd-reactor cannot "+
				"take it over: %w", am.nodeName(primary), spec.Resource, err)
		}
	} else {
		log.Debug("AppManager.Create: leaving a Primary that was not ours in place", zap.String("primary", primary))
	}
	if initErr != nil {
		return nil, initErr
	}
	reused := keyValues(out)["state"] == "reused"
	log.Debug("AppManager.Create: volume state", zap.Bool("data_reused", reused))

	app := &database.App{
		Name: spec.Name, Engine: string(spec.Engine), Resource: spec.Resource, ServiceIP: spec.ServiceIP,
		Port: spec.Port, Vector: spec.Vector, Device: device,
		Server: agreed.Binaries.Server, Client: agreed.Binaries.Client, Admin: agreed.Binaries.Admin,
		Init: agreed.Binaries.Init, Flavor: agreed.Binaries.Flavor, Version: agreed.Version,
		UID: agreed.UID, GID: agreed.GID,
	}
	// Recorded before the promoter exists, so a failure from here on leaves an
	// app `haify app delete` can find and clean up.
	start := time.Now()
	err = am.c.db.SaveApp(ctx, app)
	log.Debug("AppManager.Create: db save app", zap.String("app", app.Name), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("record the app: %w", err)
	}
	if err := am.install(ctx, spec, agreed.Binaries, device, hosts); err != nil {
		log.Debug("AppManager.Create: install failed", zap.String("app", spec.Name), zap.Error(err))
		return nil, fmt.Errorf("%w; the app is recorded, so `haify app delete %s` removes what was installed", err, spec.Name)
	}
	logger.Info("App created", zap.Bool("data_reused", reused), zap.Strings("hosts", hosts))

	created := &AppCreated{App: app, Primary: am.nodeName(primary), Reused: reused}
	if !reused {
		created.Password = password
	}
	log.Debug("AppManager.Create: done", zap.String("app", spec.Name), zap.String("primary", created.Primary),
		zap.Bool("data_reused", reused), zap.Bool("has_password", created.Password != ""))
	return created, nil
}

// checkCreatable refuses an app whose name is taken, whose resource does not
// exist, or whose resource already has a promoter of any kind: drbd-reactor
// runs one promoter per resource, and two would fight over its Primary.
func (am *AppManager) checkCreatable(ctx context.Context, spec apptemplate.Spec) error {
	db := am.c.db
	log := am.c.traceLogger()
	log.Debug("AppManager.checkCreatable: checking", zap.String("app", spec.Name), zap.String("resource", spec.Resource))
	if existing, err := db.GetApp(ctx, spec.Name); err != nil {
		log.Debug("AppManager.checkCreatable: db get app failed", zap.String("app", spec.Name), zap.Error(err))
		return err
	} else if existing != nil {
		log.Debug("AppManager.checkCreatable: name taken", zap.String("app", spec.Name), zap.String("resource", existing.Resource))
		return fmt.Errorf("app %s already exists (on resource %s)", spec.Name, existing.Resource)
	}
	if spec.Resource == SelfHaResource {
		log.Debug("AppManager.checkCreatable: refusing the metadata resource", zap.String("resource", spec.Resource))
		return fmt.Errorf("%s is the controller's own metadata resource; an app needs a resource of its own", spec.Resource)
	}
	res, err := db.GetResource(ctx, spec.Resource)
	log.Debug("AppManager.checkCreatable: db get resource", zap.String("resource", spec.Resource), zap.Bool("found", res != nil), zap.Error(err))
	if err != nil || res == nil {
		return fmt.Errorf("resource %s not found; create it first (haify resource create --name %s --size ... --nodes ...)",
			spec.Resource, spec.Resource)
	}
	if other, err := db.GetAppByResource(ctx, spec.Resource); err != nil {
		log.Debug("AppManager.checkCreatable: db get app by resource failed", zap.Error(err))
		return err
	} else if other != nil {
		log.Debug("AppManager.checkCreatable: resource already runs an app", zap.String("app", other.Name))
		return fmt.Errorf("resource %s already runs the app %s", spec.Resource, other.Name)
	}
	if ha, err := db.GetHaConfig(ctx, spec.Resource); err == nil && ha != nil {
		log.Debug("AppManager.checkCreatable: resource has an HA config", zap.String("resource", spec.Resource))
		return fmt.Errorf("resource %s already has an HA config (haify ha create); remove it first (haify ha delete %s)",
			spec.Resource, spec.Resource)
	}
	if gw, err := db.GetGatewayByResource(ctx, spec.Resource); err == nil && gw != nil {
		log.Debug("AppManager.checkCreatable: resource has a gateway", zap.String("resource", spec.Resource))
		return fmt.Errorf("resource %s already exports a gateway; delete it first (haify gateway delete %s)",
			spec.Resource, spec.Resource)
	}
	err = am.c.assertPromoterAllowed(ctx, spec.Resource, "an app")
	log.Debug("AppManager.checkCreatable: result", zap.String("resource", spec.Resource), zap.Error(err))
	return err
}

// checkNoForeignPromoter looks on the nodes themselves for a promoter the
// database does not know about: an HA config or gateway made by hand or left
// behind. Its own config left by an earlier, failed create is not foreign.
func (am *AppManager) checkNoForeignPromoter(ctx context.Context, spec apptemplate.Spec, hosts []string) error {
	own := path.Base(apptemplate.PromoterPath(spec.Name))
	script := fmt.Sprintf(`for f in %[1]s/haify-*-%[2]s.toml %[1]s/haify-*-%[2]s.toml.disabled; do
  [ -e "$f" ] || continue
  case "$(basename "$f")" in %[3]s|%[3]s.disabled) continue ;; esac
  echo "promoter=$f"
done
true
`, apptemplate.ReactorConfigDir, spec.Resource, own)
	log := am.c.traceLogger()
	log.Debug("AppManager.checkNoForeignPromoter: looking on the nodes", zap.String("resource", spec.Resource),
		zap.Strings("hosts", hosts), zap.String("own", own))
	res, err := am.runAppScript(ctx, hosts, script)
	if err != nil {
		return fmt.Errorf("look for existing promoters of %s: %w", spec.Resource, err)
	}
	var found []string
	for _, h := range hosts {
		out, ok := appHostOutput(res, h)
		log.Debug("AppManager.checkNoForeignPromoter: host answered", zap.String("host", h), zap.Bool("ok", ok))
		if !ok {
			return fmt.Errorf("%s did not answer when looking for existing promoters of %s", am.nodeName(h), spec.Resource)
		}
		for _, line := range strings.Split(out, "\n") {
			if f, ok := strings.CutPrefix(strings.TrimSpace(line), "promoter="); ok {
				found = append(found, am.nodeName(h)+":"+f)
			}
		}
	}
	log.Debug("AppManager.checkNoForeignPromoter: result", zap.Strings("found", found))
	if len(found) > 0 {
		return fmt.Errorf("resource %s already has a drbd-reactor promoter (%s); a resource runs one promoter, so remove "+
			"that one first", spec.Resource, strings.Join(found, ", "))
	}
	return nil
}

// checkPrereqs probes hosts and requires them to agree with each other and,
// when ref is given (a replica joining an existing app), with what the app
// was created with.
func (am *AppManager) checkPrereqs(ctx context.Context, spec apptemplate.Spec, hosts []string, ref *apptemplate.NodeProbe) (apptemplate.Agreed, error) {
	log := am.c.traceLogger()
	log.Debug("AppManager.checkPrereqs: probing", zap.String("app", spec.Name), zap.Strings("hosts", hosts),
		zap.Bool("has_reference", ref != nil))
	res, err := am.runAppScript(ctx, hosts, apptemplate.ProbeScript(spec))
	if err != nil {
		return apptemplate.Agreed{}, fmt.Errorf("probe the nodes: %w", err)
	}
	var nodes []apptemplate.NodeResult
	if ref != nil {
		nodes = append(nodes, apptemplate.NodeResult{Node: "the app as created", Probe: *ref})
	}
	for _, h := range hosts {
		out, ok := appHostOutput(res, h)
		if !ok {
			log.Debug("AppManager.checkPrereqs: host could not be probed", zap.String("host", h))
			return apptemplate.Agreed{}, fmt.Errorf("%s could not be probed: %s", am.nodeName(h), hostFailure(res.Hosts[h]))
		}
		p := apptemplate.ParseProbe(out)
		log.Debug("AppManager.checkPrereqs: host probe", zap.String("host", h), zap.String("version", p.Version),
			zap.Int("uid", p.UID), zap.Strings("missing", p.Missing), zap.String("port_busy", p.PortBusy))
		nodes = append(nodes, apptemplate.NodeResult{Node: am.nodeName(h), Probe: p})
	}
	agreed, err := apptemplate.CheckNodes(spec, nodes)
	log.Debug("AppManager.checkPrereqs: result", zap.String("version", agreed.Version), zap.Error(err))
	return agreed, err
}

// dataDevice is the DRBD device of the resource's first volume.
func (am *AppManager) dataDevice(ctx context.Context, resource string) (string, error) {
	log := am.c.traceLogger()
	start := time.Now()
	vols, err := am.c.db.ListVolumes(ctx, resource)
	log.Debug("AppManager.dataDevice: db list volumes", zap.String("resource", resource), zap.Int("count", len(vols)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "", fmt.Errorf("read the volumes of %s: %w", resource, err)
	}
	if len(vols) == 0 {
		return "", fmt.Errorf("resource %s has no volumes recorded", resource)
	}
	first := vols[0]
	for _, v := range vols[1:] {
		if v.VolumeID < first.VolumeID {
			first = v
		}
	}
	dev := apptemplate.DataDevice(resource, first.VolumeID)
	log.Debug("AppManager.dataDevice: chosen", zap.String("resource", resource), zap.Int("volume", first.VolumeID), zap.String("device", dev))
	return dev, nil
}

// firstPrimary picks the node to initialize on: the one already Primary, or
// the first diskful replica, promoted now. promoted says which.
func (am *AppManager) firstPrimary(ctx context.Context, resource string, hosts []string) (string, bool, error) {
	log := am.c.traceLogger()
	primary, err := am.primaryOf(ctx, resource, hosts)
	if err != nil {
		return "", false, err
	}
	if primary != "" {
		log.Debug("AppManager.firstPrimary: already Primary", zap.String("resource", resource), zap.String("primary", primary))
		return primary, false, nil
	}
	log.Debug("AppManager.firstPrimary: promoting the first replica", zap.String("resource", resource), zap.String("host", hosts[0]))
	if err := am.rm().SetPrimary(ctx, resource, hosts[0], false); err != nil {
		log.Debug("AppManager.firstPrimary: promote failed", zap.String("host", hosts[0]), zap.Error(err))
		return "", false, fmt.Errorf("promote %s on %s to initialize it: %w", resource, am.nodeName(hosts[0]), err)
	}
	return hosts[0], true, nil
}

// initialize stages the password on host and runs the initialization script
// there. The staged copy is removed by the script, and again here in case the
// script never ran.
func (am *AppManager) initialize(ctx context.Context, host string, spec apptemplate.Spec, bins apptemplate.Binaries, device, password string) (string, error) {
	log := am.c.traceLogger()
	log.Debug("AppManager.initialize: start", zap.String("app", spec.Name), zap.String("host", host),
		zap.String("device", device), zap.Bool("has_password", password != ""))
	rel := ".haify-app/" + spec.Name + ".pw"
	staged, err := am.stagePassword(ctx, host, rel, password)
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		_, rerr := am.rm().deployment.Exec(cctx, []string{host}, fmt.Sprintf(`rm -f "$HOME/%s"`, rel))
		log.Debug("AppManager.initialize: staged password removed", zap.String("host", host),
			zap.Duration("took", time.Since(start)), zap.Error(rerr))
	}()
	if err != nil {
		log.Debug("AppManager.initialize: staging the password failed", zap.String("host", host), zap.Error(err))
		return "", err
	}
	res, err := am.runAppScript(ctx, []string{host}, apptemplate.InitScript(spec, bins, device, staged))
	if err != nil {
		return "", fmt.Errorf("initialize %s on %s: %w", spec.Name, am.nodeName(host), err)
	}
	out, ok := appHostOutput(res, host)
	log.Debug("AppManager.initialize: script finished", zap.String("host", host), zap.Bool("ok", ok))
	if !ok {
		return "", fmt.Errorf("initialize %s on %s: %s", spec.Name, am.nodeName(host), hostFailure(res.Hosts[host]))
	}
	return out, nil
}

// stagePassword copies the password to host over the SSH stream into a 0600
// file in a 0700 directory of the login user's home, and returns its
// absolute path. It never appears in a command line.
func (am *AppManager) stagePassword(ctx context.Context, host, rel, password string) (string, error) {
	log := am.c.traceLogger()
	dir := path.Dir(rel)
	log.Debug("AppManager.stagePassword: preparing directory", zap.String("host", host), zap.String("dir", dir),
		zap.Bool("has_password", password != ""))
	start := time.Now()
	res, err := am.rm().deployment.Exec(ctx, []string{host},
		fmt.Sprintf(`mkdir -p "$HOME/%[1]s" && chmod 0700 "$HOME/%[1]s" && echo "HAIFY_HOME=$HOME"`, dir))
	log.Debug("AppManager.stagePassword: mkdir done", zap.String("host", host),
		zap.Strings("failed_hosts", tracedFailedHosts(res)), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "", fmt.Errorf("prepare %s on %s: %w", dir, am.nodeName(host), err)
	}
	out, ok := appHostOutput(res, host)
	home := keyValues(out)["HAIFY_HOME"]
	if !ok || !strings.HasPrefix(home, "/") {
		log.Debug("AppManager.stagePassword: no usable home directory", zap.String("host", host), zap.Bool("ok", ok), zap.String("home", home))
		return "", fmt.Errorf("prepare %s on %s: %s", dir, am.nodeName(host), hostFailure(res.Hosts[host]))
	}
	start = time.Now()
	put, err := am.rm().deployment.DistributeSecret(ctx, []string{host}, password, rel)
	log.Debug("AppManager.stagePassword: secret copied", zap.String("host", host), zap.String("path", rel),
		zap.Bool("success", put != nil && put.Success), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return "", fmt.Errorf("copy the generated password to %s: %w", am.nodeName(host), err)
	}
	if put == nil || !put.Success || len(put.FailedHosts()) > 0 {
		return "", fmt.Errorf("copy the generated password to %s failed", am.nodeName(host))
	}
	return path.Join(home, rel), nil
}

// install writes the unit and then the promoter config to hosts and reloads
// drbd-reactor there, which starts the app on whichever node promotes first.
// The unit goes first: a promoter naming a unit systemd does not know fails.
func (am *AppManager) install(ctx context.Context, spec apptemplate.Spec, bins apptemplate.Binaries, device string, hosts []string) error {
	log := am.c.traceLogger()
	log.Debug("AppManager.install: start", zap.String("app", spec.Name), zap.String("device", device), zap.Strings("hosts", hosts))
	cfg, err := apptemplate.PromoterConfig(spec, device)
	if err != nil {
		log.Debug("AppManager.install: promoter config could not be built", zap.Error(err))
		return err
	}
	dep := am.rm().deployment
	start := time.Now()
	unitRes, err := dep.DistributeConfig(ctx, hosts, apptemplate.Unit(spec, bins), apptemplate.UnitPath(spec.Name))
	log.Debug("AppManager.install: unit distributed", zap.String("path", apptemplate.UnitPath(spec.Name)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || (unitRes != nil && len(unitRes.FailedHosts()) > 0) {
		return fmt.Errorf("install %s: %v", apptemplate.UnitName(spec.Name), installFailure(unitRes, err))
	}
	if err := am.rm().execAllSuccess(ctx, hosts, "sudo systemctl daemon-reload", "reload systemd"); err != nil {
		log.Debug("AppManager.install: daemon-reload failed", zap.Error(err))
		return err
	}
	start = time.Now()
	cfgRes, err := dep.DistributeConfig(ctx, hosts, cfg, apptemplate.PromoterPath(spec.Name))
	log.Debug("AppManager.install: promoter config distributed", zap.String("path", apptemplate.PromoterPath(spec.Name)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil || (cfgRes != nil && len(cfgRes.FailedHosts()) > 0) {
		return fmt.Errorf("install the promoter config: %v", installFailure(cfgRes, err))
	}
	if res, err := am.runAppScript(ctx, hosts, reloadReactorScript); err != nil || !res.AllSuccess() {
		am.c.logger.Warn("drbd-reactor reload failed after installing an app", zap.String("app", spec.Name),
			zap.Error(err))
	}
	log.Debug("AppManager.install: done", zap.String("app", spec.Name))
	return nil
}

func installFailure(res interface{ FailedHosts() []string }, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("failed on %v", res.FailedHosts())
}
