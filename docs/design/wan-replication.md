# Haify × haify-proxy: optional WAN replication (design)

Date: 2026-07-04 (updated to match the current implementation)
Status: implemented. Opt-in only; the LAN path is unchanged.

## Guiding principle

WAN replication is opt-in. A resource created without any WAN flag behaves
exactly as a LAN resource always has: direct DRBD between real peer
addresses, the requested protocol (default C), no proxy. Every WAN code path
is gated behind `WANMode`, whose zero value is `false`.

## What WAN mode gives a resource

Replication across the internet from a primary site (possibly behind NAT) to
one DR node through one haify-proxy tunnel per WAN leg. The tunnel
provides protocol A, DRBD pull-ahead, zstd compression, mTLS, a 4 GiB buffer,
and ping-ack synthesis on the acceptor, so DRBD survives a dead WAN in `Ahead`
and recovers with a partial resync.

There are two shapes:

- Two endpoints: one primary-site node and the DR node. The whole resource is
  protocol A.
- Primary site + DR: several primary-site replicas in a synchronous LAN mesh,
  plus one asynchronous DR copy. Only the WAN legs are protocol A; the LAN
  mesh keeps the requested protocol.

## Opt-in surface

At create time (without these flags the resource is LAN):

```
haify resource create --name data --port 7000 --size 10G \
    --nodes site1-a,site1-b --wan --dr-node dr1 --dr-endpoint 203.0.113.7 \
    [--wan-port 37901] [--wan-egress-address 10.0.0.5]
```

- `--wan` is the master switch. Without it the DR flags are rejected.
- `--nodes` is required in WAN mode (no auto-placement) and lists the
  primary-site replicas.
- `--dr-node`: the DR node, which must be registered.
- `--dr-endpoint`: the DR site's public address, which the primary site dials.
  The DR is the acceptor (public side); the primary site may be behind NAT and
  dials out.
- `--wan-port`: the base mTLS port. 0 picks a random port in 3001–65535. Leg
  *i* uses base + *i*.
- `--wan-egress-address`: the source address the primary's proxy binds before
  dialing (see "Dedicated networks").

On a running resource:

- `haify resource add-dr <res> --dr-node --dr-endpoint [--wan-port]
  [--egress-address]` attaches a DR copy in place; existing replicas keep their
  synchronous mesh and a promoted resource keeps serving while the DR syncs.
- `haify wan set-endpoint <res>` changes the DR endpoint and/or egress
  address and rebuilds the tunnels. An endpoint that does not answer is refused
  unless `--skip-check`.
- `haify wan repair <res> [--dry-run]` re-provisions the legs a resource
  should have and removes instances orphaned by a renumbered or removed node.
- `haify wan tune <res> [--buffer] [--compression]` changes the proxy buffer
  and compression, moves DRBD's congestion-fill with the buffer, and restarts
  the legs.
- `haify wan sizing <res> [--bandwidth] [--days] [--sample-compression]` says
  which setup the workload needs and how big the buffer should be (below).

gRPC: `CreateResourceRequest` fields `wan`, `dr_node`, `dr_endpoint`,
`wan_port`, `wan_egress_address`; RPCs `AddDR`, `SetWanEndpoint`,
`SetWanTuning`, `GetWanSizing`, `RepairWanProxy`, `DRFailback`.

## Data model

`database.Resource` carries `WANMode`, `DRNode`, `DREndpoint`, `WANPort`,
`WANEgressAddress`, `WANBufferBytes` and `WANZstdLevel`. Records written before
these fields existed deserialize with them zero-valued, i.e. LAN, and a zero
buffer or level means the default.

## DRBD config generation

`generateDrbdConfig` (`pkg/controller/resource_drbd_config.go`) takes a
`*wanConfig`; nil renders the LAN config unchanged.

With two endpoints the resource uses protocol A, plus these `net` defaults
(user options still override them):

```
on-congestion pull-ahead; congestion-fill <90% of the proxy buffer>;
congestion-extents 500; ko-count 0; ping-timeout 20; csums-alg sha256;
```

and a resource-level `disk { c-plan-ahead 200; c-fill-target 15M; }`.

Addresses are loopback so each node talks to its local proxy:

```
on <primary> { address 127.0.0.1:<P+9>; }   # binds here, connects to 127.0.0.1:<P> = local dialer
on <dr-node> { address 127.0.0.1:<P>;   }   # local acceptor dials it here
```

With a primary site and a DR, each node's `address` is its LAN replication
address (the DR's is loopback and unused). The primary-site nodes get a
`connection-mesh`. Each primary *i* gets an explicit `connection` to the DR:

```
connection {
    host <primary-i> address 127.0.0.1:<bind port>;
    host <dr>        address 127.0.0.1:<P+i>;
    net  { protocol A; on-congestion pull-ahead; congestion-fill <90% of the buffer>;
           ko-count 0; csums-alg sha256; }
    disk { c-plan-ahead 200; c-fill-target 15M; }
}
```

### Why these options

DRBD counts every write it has sent and the peer has not acknowledged as in
flight, and the proxy's buffer is exactly that. So three DRBD settings decide
whether the buffer is used at all (`pkg/controller/wan_drbd_options.go`):

- `congestion-fill` follows the proxy buffer at 90% of it, so DRBD goes Ahead
  just before the buffer is full and never sends into a full buffer (which
  would make application writes wait). It was 2M on two-endpoint resources and
  400M on legs, against a 4 GiB buffer: on a 40 Mbit/s link a 60 MiB/s burst
  put DRBD in Ahead within three seconds while the buffer held 3 MiB.
- `ko-count 0`. DRBD drops a peer that leaves a write unacknowledged for
  `ko-count` x `timeout` (42 s by default). A backlog that takes longer than
  that to drain is not a dead peer: with the default, a burst the buffer had
  absorbed ended in "Remote failed to answer a P_BARRIER", a disconnect and a
  resync. Protocol A with pull-ahead never makes the primary wait for the DR.
- `c-fill-target 15M`, `c-plan-ahead 200` (the values LINBIT gives for DRBD
  Proxy). The resync controller keeps `c-fill-target` in flight. On a
  40 Mbit/s link with a 40 ms round trip, a resync of incompressible data runs
  at 1.8 MiB/s with DRBD's 50 KiB default and at 4.5 MiB/s, 94% of the link,
  with these.

`haify wan tune` moves `congestion-fill` with the buffer: a smaller buffer
lowers it before the proxies restart, a larger one raises it after. Both are
applied with `drbdadm adjust`, which changes them on live connections. A
resource created by an earlier version is brought up to date by running
`haify wan tune <res>` with any setting, even an unchanged one.

The bind port starts at `P + 100 + i` and is probed on the host for a free
loopback port (`pickWANBindPorts`), because a fixed offset collides with
another resource whose DRBD port sits one offset away.

The generated config uses `quorum majority` so that the initial
force-promote is not blocked. Once the peers are up the controller rewrites
quorum to a majority of the primary site (`applyLocalSiteQuorum`), so the DR
does not vote on whether the primary site may write. `add-dr` applies the same
rule, so attaching a DR does not raise the bar for the local nodes.

## haify-proxy orchestration — `pkg/wanproxy`

The package is called only for WAN resources. It uses the dispatch/SSH deploy
layer through a small `DeploymentClient` interface.

1. PKI: one shared CA and one leaf (SAN `haify-proxy`, server and client EKU),
   generated once, cached on the controller under
   `/var/lib/haify/wanproxy-pki`, valid for ten years, and distributed to every
   WAN node. Each proxy pins its peer to the SAN.
2. Legs: `MultiSpec.Legs()` expands a resource into one `ProxySpec` per
   primary-site node. Leg *i* uses WAN port base + *i* and loopback DRBD port
   P + *i* on both ends. A single-leg resource keeps the per-resource name
   (`haify-proxy@<resource>`); multi-leg resources use
   `haify-proxy@<resource>_<node>`, named after the node's registered name, not
   its address, so renumbering a node does not orphan its tunnel.
3. Config: the dialer (primary) gets `drbd_listen = 127.0.0.1:<P+i>`,
   `peer = <dr-endpoint>:<wan-port>`, optional `bind_addr = <egress>:0`.
   The acceptor (DR) gets `wan_listen = 0.0.0.0:<wan-port>`,
   `drbd_listen = 127.0.0.1:<P+i>`, `acceptor_park_secs = 30`,
   `synthesize_ping_acks = true`. Both get `on_congestion = "pull-ahead"`,
   the record's buffer and compression (`bulk_cap_bytes`, default 4 GiB, at
   most 10 GiB since DRBD will not keep more in flight; `zstd_level`, default
   3, 0 for off), and metrics every 5 s.
4. Deploy: push the `haify-proxy` binary to `/usr/local/bin/haify-proxy`,
   certs and config to `/etc/haify-proxy/`, and the `haify-proxy@.service`
   template, then enable and start each instance. The controller picks a binary
   per node architecture (`/usr/local/bin/haify-proxy-<goarch>` beside the
   default), and assumes the binary is pre-staged when it has none for that
   architecture.
5. Reachability: after the acceptor starts, the primary probes the DR's WAN
   port over TCP (with retries). The package cannot open cloud security groups;
   the operator must allow the TCP port. UDP is not used.
6. Lifecycle: proxies are provisioned before `drbdadm up`, and on delete they
   are removed after `drbdadm down`. A failed create deprovisions what it
   provisioned.

## HA / DR semantics

- WAN legs are protocol A. The DR can lag by whatever is buffered, so
  failover to the DR is manual, never automatic: auto-promoting an async
  secondary that may be behind risks data loss. The DR node therefore never
  gets a promoter: `ha create`, gateways, `add-replica`/`remove-replica` and
  `resource repair` place promoters on the primary-site replicas only, and
  `resource repair` retires one that an older version gave to a DR node.
  drbd-reactor still provides HA within the primary site.
- `haify resource dr-failover <res> --yes` force-promotes the DR node after
  printing the data-loss warning. Without `--yes` it only prints the warning.
- `haify resource dr-failback <res> [--node] [--wait]` returns to the primary
  site in phases and can be re-run until it reports done: it rejoins the
  primary-site nodes (discarding what they wrote after the failover in favour
  of the DR's copy), waits for the resync from the DR, then makes the primary
  site Primary again.
- One DR node per resource.
- `haify resource status` shows WAN mode, the DR endpoint, each leg's
  `haify-proxy` unit state on both ends, whether the DR's WAN port is reachable,
  and the primary's proxy counters. haify-proxy writes those counters to
  `/run/haify-proxy/<leg>.json` (a file read over SSH, so no extra listening port
  on a WAN-facing host). `buffer_used_bytes` is the amount a DR failover would
  lose; throughput, achieved compression ratio, reconnect count and ring-full
  events are shown alongside. An unreadable snapshot renders as unknown, never
  as zero. haify-proxy 0.2 adds, sampled every second since it started: input
  rate (mean, 99th percentile, busiest second), largest backlog, the wire rate
  while a backlog waited for the link (the link's bandwidth, measured without
  sending anything extra), the longest burst, and its CPU time. The health poll
  exports them as `haify_wan_*` gauges.

## Sizing (`pkg/wansizing`, `GetWanSizing`)

`haify wan sizing <res>` applies four rules to the best figures it has: the
link carries the 99th-percentile burst (`link_sufficient`); it carries the
average and the buffer absorbs the bursts (`buffer`); compression brings the
average under it (`compress`); or nothing does and snapshots should be shipped
instead (`async_backup`). The inputs, each reported with its source and window:

- write rates: the controller's own history (`write_history.go`), one rate per
  health poll from DRBD's written counter, kept per UTC day for 14 days in the
  database, for every resource, so a LAN resource can be sized before it goes on
  the WAN; and the proxy's per-second figures for a WAN resource. The average
  comes from whichever covers more time, the burst rate is the larger 99th
  percentile;
- compression ratio: what the proxy achieved, or `--sample-compression`, which
  reads 16 chunks of 4 MiB off the backing device on one replica and runs
  `zstd -3` on them, leaving out all-zero (never written) chunks;
- link: `--bandwidth`, or the proxy's measurement once it has 10 saturated
  seconds.

The buffer recommendation is twice the larger of the backlog the proxy has
actually held and (burst rate - drain rate) x the longest burst, rounded up to
256 MiB and capped at 10 GiB. The answer also gives how long the configured
buffer holds writes during an outage and how long a full buffer takes to drain:
closing the device (unmount, VM shutdown, a planned switchover) waits for the
backlog.
- `haify resource tls` (DRBD kernel TLS) is refused for WAN resources: the
  WAN legs already run mutual TLS in haify-proxy.

## Dedicated networks

- LAN replication network: `haify node register --address <mgmt>
  [--replication-address <repl>]`. SSH uses `--address`; generated `.res`
  files point DRBD at `--replication-address`. Omitted, replication shares the
  management address. Existing resources keep their `.res` until re-rendered.
- WAN egress: `--wan-egress-address` (create) / `--egress-address`
  (`add-dr`, `wan set-endpoint`) renders as `bind_addr = "<ip>:0"` in the dialer
  config, so replication leaves over a chosen uplink and reaches the DR firewall
  from a predictable source IP. Port 0 lets the kernel pick an ephemeral port; a
  fixed one would fail with EADDRINUSE on reconnect during TIME_WAIT. The
  address is persisted on the resource record because the proxy config is
  re-rendered from it.

## Not built

- Per-resource PKI or certificate rotation (the shared CA is the only mode).
- An active link-bandwidth test: the proxy measures the link only when it is
  the bottleneck, and otherwise the operator gives `--bandwidth`.
- Staying Connected through a link that goes silent without a reset: the proxy
  answers DRBD's pings itself only once it has noticed the link is down (a
  write timeout, 30 s), and DRBD's ping times out first (2 s), so DRBD
  disconnects and resyncs the changed blocks when the link returns.
- Automatic cross-WAN failover.
- More than one DR node per resource.
