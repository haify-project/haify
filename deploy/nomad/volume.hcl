# A replicated volume: `nomad volume create volume.hcl`.
id        = "pgdata"
name      = "pgdata"
type      = "csi"
plugin_id = "haify"

capacity_min = "10GiB"
capacity_max = "10GiB"

capability {
  access_mode     = "single-node-writer"
  attachment_mode = "file-system"
}

mount_options {
  fs_type = "ext4"
}

parameters {
  pool     = "vg0"
  replicas = "2"
}
