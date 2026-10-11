# XCP-ng storage on Haify

`HaifySR` is an XCP-ng storage driver. Each virtual disk (VDI) is a Haify
resource of its own, replicated by DRBD across your hosts, so a VM keeps its
disks when a host fails, and every host of the pool can run it: a host without
a copy of a disk reads and writes it over the network.

XAPI decides where a disk is in use, and the driver follows it: the host that
starts a VM becomes its disks' DRBD Primary, and a live migration hands the
role to the destination after the source lets go, so two hosts never hold a
disk at once. A disk that loses quorum suspends I/O instead of failing it, and
the VM carries on when quorum returns.

## Requirements

- XCP-ng 8.3, with every host of the pool a registered Haify node under its
  short host name (`haify node register --name <host> --address <ip>`).
- DRBD 9 in dom0, from the XCP-ng LINSTOR repository:

  ```bash
  yum install -y xcp-ng-release-linstor
  rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-xcpng
  yum install -y --enablerepo=xcp-ng-linstor kmod-drbd drbd-utils drbd-udev
  ```

- dom0's firewall open for DRBD and, on the controller's hosts, the Haify API.
  Add before the final `REJECT` line of `/etc/sysconfig/iptables`, and to the
  running rules with `iptables -I RH-Firewall-1-INPUT <its position>`:

  ```
  -A RH-Firewall-1-INPUT -m conntrack --ctstate NEW -m tcp -p tcp --dport 7000:8999 -j ACCEPT
  -A RH-Firewall-1-INPUT -m conntrack --ctstate NEW -m tcp -p tcp --dport 3374:3376 -j ACCEPT
  ```

- With API authentication on, the token in `/etc/haify/token` on every host.

The Haify controller can run in dom0 of one of the hosts, or anywhere else.

## Install

On every host of the pool:

```bash
./install.sh
```

It copies the driver to `/opt/xensource/sm`, adds `haify` to xapi's
`sm-plugins` and restarts the toolstack, which leaves running VMs alone.

## Create the SR

```bash
xe sr-create type=haify shared=true content-type=user name-label="Haify" \
    device-config:controller=192.168.1.10 \
    device-config:pool=vg0 \
    device-config:replicas=2
```

| device-config | |
| --- | --- |
| `controller` | The controller's REST address, `host[:port]` (port 3375); with Self-HA, its floating IP |
| `pool` | The Haify pool disks are made in |
| `replicas` | Copies of each disk, default 2 |
| `token-file` | Where the API token is on each host, default `/etc/haify/token` |

## What it does

| | |
| --- | --- |
| Create, delete, resize (grow) | One Haify resource per disk, sized exactly |
| Start, stop, live migration | The running host is the disk's Primary |
| Snapshot, clone | A full, replicated copy: an instant LVM snapshot of the disk filled into a new resource on the same hosts |
| Scan | Disks the controller has for this SR appear in XAPI, and disks it no longer has disappear |

Snapshots and clones copy the whole disk, so they take as long as reading it
once, and Xen Orchestra's incremental backups, which need VHD or qcow2 images,
copy the full disk every time.

## Uninstall

Destroy the Haify SRs first, then on every host:

```bash
./install.sh --uninstall
```
