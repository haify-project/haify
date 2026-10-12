package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes/fake"
)

func TestLeaseNamespace(t *testing.T) {
	t.Setenv("NAMESPACE", "from-env")
	if got := leaseNamespace("from-flag"); got != "from-flag" {
		t.Errorf("flag given: got %q", got)
	}
	if got := leaseNamespace(""); got != "from-env" {
		t.Errorf("env set: got %q", got)
	}
}

func TestLeaseIdentityPrefersPodName(t *testing.T) {
	t.Setenv("POD_NAME", "haify-csi-controller-abc")
	if got := leaseIdentity(); got != "haify-csi-controller-abc" {
		t.Errorf("got %q", got)
	}
}

// Two replicas competing for the Lease: exactly one runs the reporter.
func TestRunWhileLeaderOneHolder(t *testing.T) {
	kube := fake.NewClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var running atomic.Int32
	run := func(c context.Context) {
		running.Add(1)
		<-c.Done()
		running.Add(-1)
	}
	for _, pod := range []string{"pod-a", "pod-b"} {
		go runWhileLeader(ctx, kube, "kube-system", pod, zap.NewNop(), run)
	}

	deadline := time.Now().Add(5 * time.Second)
	for running.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if n := running.Load(); n != 1 {
		t.Fatalf("%d replicas running the reporter, want 1", n)
	}
}
