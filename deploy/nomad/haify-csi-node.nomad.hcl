# The Haify CSI node plugin, on every client: brings a volume up on the host
# that runs the allocation (as a diskless client when the host holds no
# replica), formats it once and mounts it. Each client must be a host DRBD 9
# runs on; the plugin registers it with haify-controller under its Nomad node
# name.
job "haify-csi-node" {
  type = "system"

  group "node" {
    task "plugin" {
      driver = "docker"

      config {
        image      = "haify-csi:latest"
        command    = "/usr/local/bin/haify-csi"
        privileged = true
        args = [
          "node",
          "--endpoint=unix:///csi/csi.sock",
          "--haify-controller=192.168.1.10:3374",
        ]
        mount {
          type   = "bind"
          source = "/sys/fs/cgroup"
          target = "/sys/fs/cgroup"
        }
      }

      env {
        NODE_NAME = "${node.unique.name}"
        NODE_IP   = "${attr.unique.network.ip-address}"
      }

      template {
        data        = "HAIFY_TOKEN={{ with nomadVar \"nomad/jobs\" }}{{ .token }}{{ end }}"
        destination = "secrets/haify.env"
        env         = true
      }

      csi_plugin {
        id        = "haify"
        type      = "node"
        mount_dir = "/csi"
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
