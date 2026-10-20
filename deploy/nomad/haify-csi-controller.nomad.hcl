# The Haify CSI controller plugin: creates, deletes, snapshots and expands
# volumes through haify-controller. Two instances, so volume operations keep
# working while one of their hosts is down.
#
# Set the controller address below. With API authentication on, store the
# token where both plugin jobs can read it:
#   nomad var put nomad/jobs token=@/etc/haify/token
job "haify-csi-controller" {
  type = "service"

  group "controller" {
    count = 2

    constraint {
      distinct_hosts = true
    }

    task "plugin" {
      driver = "docker"

      config {
        image   = "haify-csi:latest"
        command = "/usr/local/bin/haify-csi"
        args = [
          "controller",
          "--endpoint=unix:///csi/csi.sock",
          "--haify-controller=192.168.1.10:3374",
        ]
      }

      template {
        data        = "HAIFY_TOKEN={{ with nomadVar \"nomad/jobs\" }}{{ .token }}{{ end }}"
        destination = "secrets/haify.env"
        env         = true
      }

      csi_plugin {
        id        = "haify"
        type      = "controller"
        mount_dir = "/csi"
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
