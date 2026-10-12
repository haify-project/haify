package mcpauth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Limiter locks out an address that keeps presenting bad credentials.
//
// The tokens are 256 random bits, so guessing one is not a realistic attack;
// what the limit is for is the cheaper mistakes — a client retrying a revoked
// token in a loop, a scanner walking the endpoints — which it turns from a
// stream of log lines and file reads into a 429.
type Limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	hits map[string][]time.Time
}

// NewLimiter allows max failures per address within window.
func NewLimiter(max int, window time.Duration) *Limiter {
	zap.L().Debug("NewLimiter: creating failure limiter", zap.Int("max", max), zap.Duration("window", window))
	return &Limiter{max: max, window: window, now: time.Now, hits: map[string][]time.Time{}}
}

func (l *Limiter) recent(key string) []time.Time {
	cut := l.now().Add(-l.window)
	h := l.hits[key]
	i := 0
	for i < len(h) && !h[i].After(cut) {
		i++
	}
	h = h[i:]
	if len(h) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = h
	}
	return h
}

// Blocked reports whether key has used up its failures.
func (l *Limiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(l.recent(key))
	blocked := n >= l.max
	if blocked {
		zap.L().Debug("Limiter.Blocked: address has used up its failures",
			zap.String("client_ip", key), zap.Int("failures", n), zap.Int("max", l.max))
	}
	return blocked
}

// Fail records one failure for key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recent(key)
	l.hits[key] = append(l.hits[key], l.now())
	zap.L().Debug("Limiter.Fail: recorded failure", zap.String("client_ip", key),
		zap.Int("failures", len(l.hits[key])), zap.Int("max", l.max))
	// A scan from many addresses must not grow this without bound.
	if len(l.hits) > 10000 {
		before := len(l.hits)
		for k := range l.hits {
			if len(l.recent(k)) == 0 {
				delete(l.hits, k)
			}
		}
		zap.L().Debug("Limiter.Fail: pruned stale addresses", zap.Int("before", before), zap.Int("after", len(l.hits)))
	}
}

// ClientIP is the address a request came from. Behind a reverse proxy the
// socket peer is the proxy for everyone, so with trustProxy the first
// X-Forwarded-For entry is used instead; without it that header is ignored,
// because anyone can send it.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			ip := strings.TrimSpace(strings.Split(xff, ",")[0])
			zap.L().Debug("ClientIP: using X-Forwarded-For", zap.String("client_ip", ip), zap.String("remote_addr", r.RemoteAddr))
			return ip
		}
		zap.L().Debug("ClientIP: trusting proxy but no X-Forwarded-For, using socket peer", zap.String("remote_addr", r.RemoteAddr))
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		zap.L().Debug("ClientIP: remote address has no port, using it whole", zap.String("remote_addr", r.RemoteAddr), zap.Error(err))
		return r.RemoteAddr
	}
	return host
}
