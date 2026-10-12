package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/haify-project/haify/pkg/apptemplate"
	"github.com/haify-project/haify/pkg/database"
)

// List returns every app.
func (am *AppManager) List(ctx context.Context) ([]*database.App, error) {
	log := am.c.traceLogger()
	if am.c.db == nil {
		log.Debug("AppManager.List: no database")
		return nil, fmt.Errorf("database not available")
	}
	start := time.Now()
	apps, err := am.c.db.ListApps(ctx)
	log.Debug("AppManager.List: db list apps", zap.Int("count", len(apps)), zap.Duration("took", time.Since(start)), zap.Error(err))
	return apps, err
}

// AppStatus is where an app runs and whether it answers there.
type AppStatus struct {
	App          *database.App
	Primary      string   // node name; empty when the resource is Primary nowhere
	ServiceState string   // systemctl is-active of the unit on the Primary
	Healthy      bool     // the engine's health probe answered
	State        string   // running, degraded or stopped
	Nodes        []string // node names the promoter runs on
}

// App states. degraded means the resource is Primary somewhere but the
// database there is not active or does not answer: starting up, recovering,
// or failing (in which case drbd-reactor moves it).
const (
	AppRunning  = "running"
	AppDegraded = "degraded"
	AppStopped  = "stopped"
)

// Status finds the node where the app's resource is Primary and asks the
// database there whether it answers.
func (am *AppManager) Status(ctx context.Context, name string) (*AppStatus, error) {
	log := am.c.traceLogger()
	log.Debug("AppManager.Status: start", zap.String("app", name))
	app, err := am.get(ctx, name)
	if err != nil {
		return nil, err
	}
	hosts, err := am.rm().failoverHosts(ctx, app.Resource)
	log.Debug("AppManager.Status: failover hosts", zap.String("resource", app.Resource), zap.Strings("hosts", hosts), zap.Error(err))
	if err != nil {
		return nil, err
	}
	st := &AppStatus{App: app, State: AppStopped}
	for _, h := range hosts {
		st.Nodes = append(st.Nodes, am.nodeName(h))
	}
	primary, err := am.primaryOf(ctx, app.Resource, hosts)
	if err != nil {
		return nil, err
	}
	if primary == "" {
		log.Debug("AppManager.Status: Primary nowhere, app stopped", zap.String("app", name))
		return st, nil
	}
	st.Primary = am.nodeName(primary)
	res, err := am.runAppScript(ctx, []string{primary}, apptemplate.StatusScript(appSpec(app), appBinaries(app)))
	if err != nil {
		return nil, fmt.Errorf("ask %s about %s: %w", st.Primary, name, err)
	}
	out, _ := appHostOutput(res, primary)
	kv := keyValues(out)
	st.ServiceState = kv["active"]
	st.Healthy = kv["health"] == "ok"
	st.State = AppDegraded
	if st.ServiceState == "active" && st.Healthy {
		st.State = AppRunning
	}
	log.Debug("AppManager.Status: result", zap.String("app", name), zap.String("primary", st.Primary),
		zap.String("service_state", st.ServiceState), zap.Bool("healthy", st.Healthy), zap.String("state", st.State))
	return st, nil
}

// Failover moves the app to another replica through drbd-reactor and returns
// the node it left and the node that took over.
func (am *AppManager) Failover(ctx context.Context, name string) (string, string, error) {
	log := am.c.traceLogger()
	log.Debug("AppManager.Failover: start", zap.String("app", name))
	app, err := am.get(ctx, name)
	if err != nil {
		return "", "", err
	}
	appOps.Lock()
	defer appOps.Unlock()
	hosts, err := am.rm().failoverHosts(ctx, app.Resource)
	log.Debug("AppManager.Failover: failover hosts", zap.String("resource", app.Resource), zap.Strings("hosts", hosts), zap.Error(err))
	if err != nil {
		return "", "", err
	}
	if len(hosts) < 2 {
		log.Debug("AppManager.Failover: no other replica", zap.Int("count", len(hosts)))
		return "", "", fmt.Errorf("resource %s has no other diskful replica to fail over to", app.Resource)
	}
	primary, err := am.primaryOf(ctx, app.Resource, hosts)
	if err != nil {
		return "", "", err
	}
	if primary == "" {
		log.Debug("AppManager.Failover: not running anywhere", zap.String("app", name))
		return "", "", fmt.Errorf("app %s is not running anywhere (%s is Primary on no node); drbd-reactor starts it "+
			"wherever it can, see haify app status %s", name, app.Resource, name)
	}
	from := am.nodeName(primary)
	res, err := am.runAppScript(ctx, []string{primary}, apptemplate.EvictScript(name))
	if err != nil {
		return from, "", fmt.Errorf("evict %s from %s: %w", name, from, err)
	}
	out, ok := appHostOutput(res, primary)
	if !ok {
		log.Debug("AppManager.Failover: evict script failed", zap.String("host", primary))
		return from, "", fmt.Errorf("evict %s from %s: %s", name, from, hostFailure(res.Hosts[primary]))
	}
	to := apptemplate.TookOver(out)
	log.Debug("AppManager.Failover: done", zap.String("app", name), zap.String("from", from), zap.String("to", to))
	am.c.logger.Info("App failed over", zap.String("app", name), zap.String("from", from), zap.String("to", to))
	return from, to, nil
}

// Delete takes the app off every node and drops its record. The resource and
// the data on it stay unless deleteData, in which case the resource is
// deleted afterwards — a step of its own, so a failure there leaves an app
// that is already gone rather than half of one.
func (am *AppManager) Delete(ctx context.Context, name string, deleteData bool) (string, error) {
	log := am.c.traceLogger()
	log.Debug("AppManager.Delete: start", zap.String("app", name), zap.Bool("delete_data", deleteData))
	app, err := am.get(ctx, name)
	if err != nil {
		return "", err
	}
	if err := am.teardown(ctx, app); err != nil {
		log.Debug("AppManager.Delete: teardown failed", zap.String("app", name), zap.Error(err))
		return "", err
	}
	msg := fmt.Sprintf("app %s deleted; resource %s and its data are kept", name, app.Resource)
	if !deleteData {
		log.Debug("AppManager.Delete: keeping the resource", zap.String("resource", app.Resource))
		return msg, nil
	}
	log.Debug("AppManager.Delete: deleting the resource too", zap.String("resource", app.Resource))
	if err := am.rm().DeleteResource(ctx, app.Resource, false); err != nil {
		log.Debug("AppManager.Delete: resource delete failed", zap.String("resource", app.Resource), zap.Error(err))
		return "", fmt.Errorf("app %s deleted, but deleting resource %s failed: %w", name, app.Resource, err)
	}
	return fmt.Sprintf("app %s and resource %s deleted", name, app.Resource), nil
}

// teardown removes the app from its nodes and the database. Every replica
// must answer: one that keeps the promoter config would start the app again
// the next time it can promote the resource. Other nodes are cleaned too, in
// case a replica was removed since, but only as a courtesy.
func (am *AppManager) teardown(ctx context.Context, app *database.App) error {
	appOps.Lock()
	defer appOps.Unlock()
	log := am.c.traceLogger()
	replicas, err := am.rm().failoverHosts(ctx, app.Resource)
	if err != nil {
		// The resource may be gone already; clean wherever the app could be.
		log.Debug("AppManager.teardown: replicas unknown, resource may be gone", zap.String("resource", app.Resource), zap.Error(err))
		replicas = nil
	}
	others := withoutAll(dedupe(am.rm().allNodeAddresses()), replicas)
	log.Debug("AppManager.teardown: start", zap.String("app", app.Name), zap.Strings("replicas", replicas), zap.Strings("others", others))

	script := apptemplate.RemoveScript(appSpec(app))
	if len(replicas) > 0 {
		res, err := am.runAppScript(ctx, replicas, script)
		if err != nil {
			return fmt.Errorf("remove app %s from its nodes: %w", app.Name, err)
		}
		var failed []string
		for _, h := range replicas {
			_, ok := appHostOutput(res, h)
			log.Debug("AppManager.teardown: replica cleaned", zap.String("host", h), zap.Bool("ok", ok))
			if !ok {
				failed = append(failed, fmt.Sprintf("%s: %s", am.nodeName(h), hostFailure(res.Hosts[h])))
			}
		}
		if len(failed) > 0 {
			log.Debug("AppManager.teardown: some replicas failed, keeping the app", zap.Int("failed", len(failed)))
			return fmt.Errorf("app %s could not be removed everywhere, so it is kept (run the delete again once "+
				"they answer): %s", app.Name, strings.Join(failed, "; "))
		}
	}
	if len(others) > 0 {
		if res, err := am.runAppScript(ctx, others, script); err != nil || !res.AllSuccess() {
			am.c.logger.Warn("Could not clean up app files on nodes that are not replicas",
				zap.String("app", app.Name), zap.Error(err))
		}
	}
	start := time.Now()
	err = am.c.db.DeleteApp(ctx, app.Name)
	log.Debug("AppManager.teardown: db delete app", zap.String("app", app.Name), zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return fmt.Errorf("drop the record of app %s: %w", app.Name, err)
	}
	am.c.logger.Info("App deleted", zap.String("app", app.Name), zap.String("resource", app.Resource))
	return nil
}

// dedupe drops empty and repeated entries, keeping the first of each.
func dedupe(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range list {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
