package csi

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// asyncOps runs the work a CSI call cannot finish within the sidecar's timeout
// — uploading a snapshot to a backup target, restoring a volume from one — in
// the background, and lets the calls the sidecar repeats ask how it went.
//
// The state is in memory only, on purpose. Each operation also leaves a
// durable trace the next call reads first (the controller's backup record, a
// label on the volume being restored), and an operation this process does not
// know about is one whose process went away mid-way: the controller saw its
// call cancelled and failed it. The callers start it again in that case.
type asyncOps struct {
	mu  sync.Mutex
	ops map[string]*asyncOp
}

type asyncOp struct {
	done   bool
	err    error
	cancel context.CancelFunc
}

// opState is what a caller learns about an operation.
type opState int

const (
	opUnknown opState = iota // never started here, or cleared
	opRunning
	opSucceeded
	opFailed
)

// start runs fn in the background under key, unless an operation under key is
// already running. The operation is not bound to the CSI call that started it
// (that call returns at once), only to cancel.
func (o *asyncOps) start(parent context.Context, key string, fn func(context.Context) error) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ops == nil {
		o.ops = map[string]*asyncOp{}
	}
	if op := o.ops[key]; op != nil && !op.done {
		zap.L().Debug("asyncOps.start: already running", zap.String("op", key))
		return false
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	op := &asyncOp{cancel: cancel}
	o.ops[key] = op
	zap.L().Debug("asyncOps.start: starting", zap.String("op", key))
	go func() {
		start := time.Now()
		err := fn(ctx)
		cancel()
		zap.L().Debug("asyncOps.start: finished", zap.String("op", key), zap.Duration("took", time.Since(start)), zap.Error(err))
		o.mu.Lock()
		op.done, op.err = true, err
		o.mu.Unlock()
	}()
	return true
}

// state reports an operation's state and, once it failed, why.
func (o *asyncOps) state(key string) (opState, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	op := o.ops[key]
	switch {
	case op == nil:
		return opUnknown, nil
	case !op.done:
		return opRunning, nil
	case op.err != nil:
		return opFailed, op.err
	default:
		return opSucceeded, nil
	}
}

// clear forgets a finished operation, so the next call starts afresh.
func (o *asyncOps) clear(key string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if op := o.ops[key]; op != nil && op.done {
		zap.L().Debug("asyncOps.clear: forgetting finished operation", zap.String("op", key))
		delete(o.ops, key)
	}
}

// cancelOp cancels a running operation; its outcome is still recorded when
// it returns.
func (o *asyncOps) cancelOp(key string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if op := o.ops[key]; op != nil && !op.done {
		zap.L().Debug("asyncOps.cancelOp: cancelling", zap.String("op", key))
		op.cancel()
	}
}
