package k8sapp

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Status is one row of GET /ai/apps.
type Status struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Template  string `json:"template"`
	Ready     bool   `json:"ready"`
	Node      string `json:"node,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Claim     string `json:"claim"`
	Volume    string `json:"volume,omitempty"` // the DRBD resource behind the claim
	Service   string `json:"service"`
}

func (m *Manager) List(ctx context.Context) ([]Status, error) {
	log := zap.L()
	sel := metav1.ListOptions{LabelSelector: managedByKey + "=" + managedByValue}
	log.Debug("Manager.List: listing apps", zap.String("selector", sel.LabelSelector))
	start := time.Now()
	deps, err := m.kube.AppsV1().Deployments("").List(ctx, sel)
	if err != nil {
		log.Debug("Manager.List: listing deployments failed", zap.Duration("took", time.Since(start)), zap.Error(err))
		return nil, err
	}
	log.Debug("Manager.List: listed deployments", zap.Int("count", len(deps.Items)), zap.Duration("took", time.Since(start)))
	out := make([]Status, 0, len(deps.Items))
	for _, d := range deps.Items {
		st := Status{Namespace: d.Namespace, Name: d.Name, Template: d.Annotations[templateKey],
			Ready: d.Status.ReadyReplicas > 0, Claim: d.Name + "-data"}
		log.Debug("Manager.List: app", zap.String("namespace", d.Namespace), zap.String("name", d.Name),
			zap.String("template", st.Template), zap.Bool("ready", st.Ready))
		if tpl, ok := templates[st.Template]; ok {
			st.Service = fmt.Sprintf("%s.%s.svc:%d", d.Name, d.Namespace, tpl.port)
		} else {
			log.Debug("Manager.List: unknown template, no service address", zap.String("name", d.Name),
				zap.String("template", st.Template))
		}
		start := time.Now()
		pods, err := m.kube.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/instance=" + d.Name + "," + managedByKey + "=" + managedByValue})
		log.Debug("Manager.List: listed pods", zap.String("name", d.Name), zap.Duration("took", time.Since(start)),
			zap.Error(err))
		if err == nil {
			for _, p := range pods.Items {
				if p.DeletionTimestamp != nil {
					log.Debug("Manager.List: skipping terminating pod", zap.String("pod", p.Name),
						zap.String("node", p.Spec.NodeName))
					continue // the pod left behind on a failed node
				}
				st.Node, st.Phase = p.Spec.NodeName, string(p.Status.Phase)
				log.Debug("Manager.List: pod", zap.String("pod", p.Name), zap.String("node", st.Node),
					zap.String("phase", st.Phase))
			}
		}
		start = time.Now()
		pvc, err := m.kube.CoreV1().PersistentVolumeClaims(d.Namespace).Get(ctx, st.Claim, metav1.GetOptions{})
		log.Debug("Manager.List: got claim", zap.String("claim", st.Claim), zap.Bool("found", err == nil),
			zap.Duration("took", time.Since(start)), zap.Error(err))
		if err == nil && pvc.Spec.VolumeName != "" {
			start = time.Now()
			pv, err := m.kube.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
			log.Debug("Manager.List: got volume", zap.String("volume", pvc.Spec.VolumeName), zap.Bool("found", err == nil),
				zap.Duration("took", time.Since(start)), zap.Error(err))
			if err == nil && pv.Spec.CSI != nil {
				st.Volume = pv.Spec.CSI.VolumeHandle
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})
	log.Debug("Manager.List: done", zap.Int("count", len(out)))
	return out, nil
}
