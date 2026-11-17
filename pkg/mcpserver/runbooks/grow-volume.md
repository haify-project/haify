# Grow a volume
Make a resource's volume larger, and use the space.
Needs: operate

1. `haify_resource_status`: every replica connected and UpToDate. A resize is done on all nodes at once.
2. `haify_pool_list`: each replica's pool has room for the growth. The added area is resynced onto every replica, so on a thin pool the growth becomes used space on each node — and a full pool drops that replica's disk. The resize is refused when a replica's LVM pool is known to have less free space than the growth, and names it; ZFS volumes are not checked. Only the CLI can override that (`--ignore-free-space`), this tool cannot. On a thin pool count snapshots too.
3. `haify_resource_resize_volume` with the volume ID and the new total size in GiB. Shrinking is not supported.
4. Read the result's detail. For a resource with an NFS or SMB gateway it says the gateway's filesystem was grown, on the node serving it; nothing more is needed. For a resource a libvirt guest uses it says whether the running guest was shown the new size, and the `virsh blockresize` command to run when it was not.
5. An error saying the volume is grown but its filesystem is not means the gateway was failing over or stopped at that moment. Check `haify_gateway_get`, start it with `haify_gateway_start` if it is stopped, and repeat the same call: it grows only the filesystem.
6. Any other filesystem on the volume is not grown by step 3. Either repeat the call with `grow_filesystem` set, which grows the filesystem mounted from the volume on the Primary, or on the Primary node run `resize2fs /dev/drbd<minor>` for ext4 (online) or `xfs_growfs <mountpoint>` for XFS. The Primary and the device come from `haify_resource_status` and `haify_resource_list`.
7. `df` on the mount shows the new size.

The backing volume is grown on every replica's node before DRBD is resized; a node that cannot be reached fails the call. DRBD refuses a resize while a resync is running, such as a volume's initial sync. The backing volumes stay grown then: wait for the sync to finish and repeat the same call.
