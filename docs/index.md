---
template: home.html
title: Storage that keeps working when a server fails
hide:
  - navigation
  - toc
---

<!-- The site renders this page from docs-site/overrides/home.html, which
     shows none of the text below; it is for reading docs/ in the repository.
     Keep the two lists in step. -->

- [Deployment](deployment-guide.md): install Haify and set up a cluster
- [User guide](user-guide.md): everyday tasks, step by step
- [Node prerequisites](node-prerequisites.md): what each server needs
- [Kubernetes (CSI)](../deploy/k8s/README.md): volumes for containers, also as a [Helm chart](../deploy/helm/haify-csi/README.md); [DRBD on Kubernetes nodes](./kubernetes-drbd.md) covers loading the kernel module from the cluster
- [Nomad](../deploy/nomad/README.md): the CSI driver as Nomad plugin jobs
- [Docker and Podman](../deploy/docker/README.md): volumes that follow containers between hosts
- [Proxmox VE](../deploy/proxmox/README.md): volumes for virtual machines
- [XCP-ng](../deploy/xcp-ng/README.md): a storage repository whose every disk is replicated
- [KVM (libvirt)](../deploy/libvirt/README.md): VM disks under libvirt, with failover
- [OpenStack](../deploy/openstack/README.md): Cinder volumes, and a panel in Horizon
- [OpenNebula](../deploy/opennebula/README.md): images on replicated volumes, with live migration and host-failure restarts
- [Evacuating VMware vSAN](vsan-evacuation.md): move VMware VMs to Proxmox VE on Haify
- [AI assistants (MCP)](mcp.md): manage Haify by asking
