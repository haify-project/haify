package deployment

import (
	"context"
	"time"

	"github.com/liliang-cn/dispatch/pkg/dispatch"
	"go.uber.org/zap"
)

// WithReadOnly marks a command that only reads. Exec then honours ctx: if ctx
// ends before the hosts answer, Exec returns ctx's error at once, and the
// command's timeout is cut to what ctx has left.
//
// dispatch looks at ctx only before it starts. Past that it waits out its own
// SSH dial timeout (30s) and the command timeout, so a caller that gave a
// status read five seconds, because another replica could answer instead,
// still waited half a minute on a powered-off node. Abandoning the call is
// only safe for a read: the command may still run to its end in the
// background, and nothing waits for a result it cannot change.
func WithReadOnly() ExecOption {
	return func(o *execOptions) {
		o.readOnly = true
	}
}

type dispatchReply struct {
	result *dispatch.ExecResult
	err    error
}

// dispatchExec runs cmd on the remote hosts through dispatch, returning early
// on ctx for a read-only command.
func (c *Client) dispatchExec(ctx context.Context, hosts []string, cmd string, parallel int, timeout time.Duration, readOnly bool) (*dispatch.ExecResult, error) {
	log := c.log()
	if !readOnly {
		return c.dispatch.Exec(ctx, hosts, cmd, dispatch.WithParallel(parallel), dispatch.WithTimeout(timeout))
	}
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < timeout {
			timeout = max(left, time.Second)
			log.Debug("dispatchExec: timeout cut to the caller's deadline", zap.Strings("hosts", hosts), zap.Duration("timeout", timeout))
		}
	}
	reply := make(chan dispatchReply, 1)
	go func() {
		r, err := c.dispatch.Exec(ctx, hosts, cmd, dispatch.WithParallel(parallel), dispatch.WithTimeout(timeout))
		reply <- dispatchReply{r, err}
	}()
	select {
	case r := <-reply:
		return r.result, r.err
	case <-ctx.Done():
		log.Debug("dispatchExec: caller gave up, not waiting for the hosts", zap.Strings("hosts", hosts), zap.Error(ctx.Err()))
		return nil, ctx.Err()
	}
}
