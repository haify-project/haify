resource tgl {

    options {
        auto-promote no;
        on-no-data-accessible io-error;
        on-no-quorum io-error;
        on-suspended-primary-outdated force-secondary;
        quorum majority;
    }

    net {
        protocol C;
        verify-alg crc32c;
        rr-conflict retry-connect;
    }

    volume 0 {
        device    minor 12;
        disk      /dev/haify_vg0/tgl_data;
        meta-disk internal;
        disk {
            rs-discard-granularity 65536;
        }
    }

    on pve-a {
        address   192.168.123.111:7090;
        node-id   0;
    }

    on pve-b {
        address   192.168.123.112:7090;
        node-id   1;
    }

    on pve-c {
        address   192.168.123.113:7090;
        node-id   2;
        volume 0 {
            device    minor 12;
            disk      none;
        }
    }


    # haify-paths: mesh pve-a pve-b pve-c
    connection {
        path {
            host pve-a address 192.168.123.111:7090;
            host pve-b address 192.168.123.112:7090;
        }
        path {
            host pve-a address 10.77.0.11:7090;
            host pve-b address 10.77.0.12:7090;
        }
    }
    connection {
        path {
            host pve-a address 192.168.123.111:7090;
            host pve-c address 192.168.123.113:7090;
        }
        path {
            host pve-a address 10.77.0.11:7090;
            host pve-c address 10.77.0.13:7090;
        }
    }
    connection {
        path {
            host pve-b address 192.168.123.112:7090;
            host pve-c address 192.168.123.113:7090;
        }
        path {
            host pve-b address 10.77.0.12:7090;
            host pve-c address 10.77.0.13:7090;
        }
    }
    # haify-paths end
}
