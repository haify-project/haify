# XCP-ng storage on Haify

`HaifySR` is an XCP-ng storage driver. Each virtual disk (VDI) is its own
Haify resource, replicated by DRBD across your hosts. A VM keeps its disks when
a host fails, and any host of the pool can run it: a host without a copy of a
disk reads and writes it over the network.

XAPI decides where a disk is in use, and the driver follows it. The host that
starts a VM becomes the DRBD Primary of its disks. In a live migration the
source gives up the role first and then the destination takes it, so two
hosts never hold a disk at once. A disk that loses quorum pauses its I/O
instead of failing it, and the VM carries on when quorum returns.

## Requirements

- XCP-ng 8.3. Every host of the pool must be registered as a Haify node under
  its short host name (`haify node register --name <host> --address <ip>`).
- DRBD 9 in dom0, from the XCP-ng LINSTOR repository:

  ```bash
  yum install -y xcp-ng-release-linstor
  rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-xcpng
  yum install -y --enablerepo=xcp-ng-linstor kmod-drbd drbd-utils drbd-udev
  ```

- dom0's firewall must let DRBD through, and on the hosts that run the
  controller, the Haify API too. Add these lines before the final `REJECT`
  line of `/etc/sysconfig/iptables`. Add them to the running rules as well,
  with `iptables -I RH-Firewall-1-INPUT <its position>`:

  ```
  -A RH-Firewall-1-INPUT -m conntrack --ctstate NEW -m tcp -p tcp --dport 7000:8999 -j ACCEPT
  -A RH-Firewall-1-INPUT -m conntrack --ctstate NEW -m tcp -p tcp --dport 3374:3376 -j ACCEPT
  ```

- If API authentication is on: the token in `/etc/haify/token` on every host.

The Haify controller can run in dom0 of one of the hosts, or anywhere else.

## Install

On every host of the pool:

```bash
./install.sh
```

It copies the driver to `/opt/xensource/sm` and adds `haify` to xapi's
`sm-plugins`. It loads the DRBD module now and at every boot. Then it restarts
the toolstack, which leaves running VMs alone.

## Create the SR

```bash
xe sr-create type=haify shared=true content-type=user name-label="Haify" \
    device-config:controller=192.168.1.10 \
    device-config:pool=vg0 \
    device-config:replicas=2
```

| device-config | |
| --- | --- |
| `controller` | The controller's REST address, `host[:port]` (default port 3375). It takes one address, so under Self-HA give the floating IP |
| `pool` | The Haify pool disks are made in |
| `replicas` | How many copies of each disk, default 2 |
| `token-file` | Where the API token is on each host, default `/etc/haify/token` |

## What it does

| | |
| --- | --- |
| Create, delete, resize (grow) | One Haify resource per disk. The size is rounded up to a whole MiB. A disk can grow but not shrink |
| Start, stop, live migration | The host running the VM is the disk's Primary. A host without a copy joins the disk over the network the first time it needs it, and stays joined after the VM stops or leaves |
| Snapshot, clone | A full, replicated copy. Haify snapshots the disk on every copy at the same instant, copies the snapshot into a new resource on the same hosts, then deletes it. This works on LVM and ZFS pools |
| Scan | Disks the controller has for this SR appear in XAPI; disks it no longer has disappear |

Snapshots and clones copy the whole disk, so each takes as long as reading
the disk once. Xen Orchestra's incremental backups need VHD or qcow2 images,
so on this SR they copy the full disk every time.

## Uninstall

Destroy the Haify SRs first, then on every host:

```bash
./install.sh --uninstall
```
