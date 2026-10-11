# Docker volumes on Haify

`haify-docker` is a Docker volume plugin. A volume made with it is a Haify
resource, replicated across your servers, and a container that uses it can run
on any host: in a Swarm, a service that moves to another node finds its data
there.

## Install

On every Docker host, which must be a registered Haify node (`haify node list`)
with DRBD 9 and `drbd-utils`:

```bash
make build                                         # or GOOS=linux go build ./cmd/haify-docker
sudo install -m 0755 bin/haify-docker /usr/local/bin/
sudo install -m 0644 deploy/docker/haify-docker.service /etc/systemd/system/
echo 'HAIFY_DOCKER_ARGS="--controller 192.168.1.10:3374 --pool vg0"' | sudo tee /etc/default/haify-docker
sudo systemctl daemon-reload && sudo systemctl enable --now haify-docker
```

`--controller` is the controller's address (its floating IP with Self-HA). With
API authentication on, put the token in `/etc/haify/token`. `--node` defaults
to the short host name and must match the host's Haify node name.

## Use

```bash
docker volume create -d haify -o size=20G pgdata
docker run -d -v pgdata:/var/lib/postgresql/data postgres:17

# Swarm
docker service create --mount type=volume,source=pgdata,target=/var/lib/postgresql/data,volume-driver=haify postgres:17
```

Options: `size` (default `--size`, 10G), `pool` (default `--pool`), `replicas`
(default `--replicas`, 2), `fs` (`ext4` or `xfs`). A host without a replica of
the volume reads and writes over the network.

A volume is in use on one host at a time: while a container on one host has it,
starting one on another host with the same volume is refused. `docker volume
rm` deletes the volume and its data on every node.
