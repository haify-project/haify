# Docker volumes on Haify

`haify-docker` is a Docker volume plugin. Each volume made with it is a Haify
resource, replicated across your servers. A container that uses the volume can
run on any host. In a Swarm, a service that moves to another node finds its
data there.

## Install

On every Docker host, which must be a registered Haify node (`haify node list`)
with DRBD 9 and `drbd-utils`:

```bash
make build                                         # or GOOS=linux go build -o bin/haify-docker ./cmd/haify-docker
sudo install -m 0755 bin/haify-docker /usr/local/bin/
sudo install -m 0644 deploy/docker/haify-docker.service /etc/systemd/system/
echo 'HAIFY_DOCKER_ARGS="--controller 192.168.1.10:3374 --pool vg0"' | sudo tee /etc/default/haify-docker
sudo systemctl daemon-reload && sudo systemctl enable --now haify-docker
```

`--controller` is the controller's address (its floating IP with Self-HA). If
the controller has API authentication on, put the token in `/etc/haify/token`.
`--node` is this host's Haify node name and must match it exactly. It defaults
to the short host name.

## Use

```bash
docker volume create -d haify -o size=20G pgdata
docker run -d -v pgdata:/var/lib/postgresql/data postgres:17

# Swarm
docker service create --mount type=volume,source=pgdata,target=/var/lib/postgresql/data,volume-driver=haify postgres:17
```

Options: `size` (default `--size`, 10G), `pool` (default `--pool`), `replicas`
(default `--replicas`, 2), `fs` (`ext4` or `xfs`; default `--fs`, ext4). On a
host that holds no replica of the volume, the plugin joins it as a diskless
client and the host reads and writes over the network.

Containers on the same host can share a volume. Only one host can use a volume
at a time: while a container on one host has it, starting a container with the
same volume on another host is refused, with an error naming the host that has
it. `docker volume rm` deletes the volume and its data on every node.

## Sharing data between hosts

For containers on several hosts that read and write the same files at once,
put an NFS gateway on a Haify resource and mount it with Docker's own `local`
driver on each host:

```bash
haify gateway nfs create --resource shared --service-ip 192.168.1.200/24 --export-path /shared
docker volume create -d local -o type=nfs -o o=addr=192.168.1.200,rw,nfsvers=4.2 -o device=:/shared shared
docker run --rm -v shared:/data busybox:1.36 ls /data
```

When the node serving the gateway goes away, another replica takes over the
service IP and the containers carry on. Writes pause while that happens,
for around 15 to 20 seconds.
