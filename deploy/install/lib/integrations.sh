# shellcheck shell=bash
# shellcheck disable=SC2153 # NODES, ENV and the other globals live in lib.sh and haify-install.sh
# Step 7: plug the storage into the platform. What can be installed over SSH
# is installed; what needs the platform's own tooling and an image or a
# config only the operator can place (Kubernetes, Nomad, OpenStack) is
# written to OUT_DIR, filled in, with the commands that apply it.

STAGING=/root/.haify-install

# controller_ip: the one address platforms that take a single controller are
# given: the VIP under Self-HA, else the controller node.
controller_ip() {
	if [ "$SELF_HA" = 1 ]; then
		printf '%s' "${VIP%/*}"
	else
		node_ip "$CONTROLLER_NODE"
	fi
}

# controller_rest_list: every node that can run the controller, for clients
# that try a list in turn (the libvirt hook).
controller_rest_list() {
	local n out=()
	for n in "${CONTROLLER_NODES[@]}"; do out+=("$(rest_addr "$(node_ip "$n")")"); done
	join_by , "${out[@]}"
}

nodes_with() {
	local env="$1" n
	for n in "${NODES[@]}"; do
		in_list "$env" ${NODE_ENVS[$n]} && printf '%s\n' "$n"
	done
	return 0
}

stage_dir() {
	copy_dir_to "$1" "$2" "$STAGING/$(basename "$2")"
}

integrate_libvirt() {
	local n hosts=()
	mapfile -t hosts < <(nodes_with libvirt)
	[ "${#hosts[@]}" -gt 0 ] || { note "no node runs libvirt"; return 0; }
	for n in "${hosts[@]}"; do
		if check_on "$n" "test -x /etc/libvirt/hooks/qemu.d/haify" >/dev/null; then
			note "$n: libvirt hook installed"
			continue
		fi
		stage_dir "$n" "$DEPLOY_DIR/libvirt"
		run_on "$n" "cd $STAGING/libvirt && ./install.sh --controller $(q "$(controller_rest_list)") --node $(q "$n")"
	done
	note "guest disks: /dev/drbd/by-res/<resource>/0 with cache='none'; HA for a guest: haify ha create <resource> --vm <guest>"
}

integrate_opennebula() {
	local n fe=() addr
	integrate_libvirt
	for n in "${NODES[@]}"; do [ -n "${NODE_FRONTEND[$n]:-}" ] && fe+=("$n"); done
	[ "${#fe[@]}" -gt 0 ] || { warn "no node is an OpenNebula frontend (/etc/one/oned.conf); install the drivers there with deploy/opennebula/install.sh"; return 0; }
	addr=$(controller_ip)
	for n in "${fe[@]}"; do
		if ! check_on "$n" "grep -q haify /etc/one/oned.conf" >/dev/null; then
			stage_dir "$n" "$DEPLOY_DIR/opennebula"
			copy_to "$n" "$BIN_DIR/haify-opennebula" "$STAGING/haify-opennebula" 0755
			run_on "$n" "cd $STAGING/opennebula && ./install.sh --binary $STAGING/haify-opennebula && sudo -iu oneadmin onehost sync -f"
		else
			note "$n: OpenNebula drivers installed"
		fi
		if check_on "$n" "sudo -iu oneadmin onedatastore list --csv -l NAME | grep -qx haify_images" >/dev/null; then
			note "$n: datastores haify_system and haify_images exist"
			continue
		fi
		run_on "$n" "printf '%s\\n' 'NAME = \"haify_system\"' 'TYPE = \"SYSTEM_DS\"' 'TM_MAD = \"haify\"' > /tmp/haify-system.tpl && printf '%s\\n' 'NAME = \"haify_images\"' 'DS_MAD = \"haify\"' 'TM_MAD = \"haify\"' 'DISK_TYPE = \"BLOCK\"' 'HAIFY_CONTROLLER = \"$addr\"' 'HAIFY_POOL = \"$POOL\"' 'HAIFY_REPLICAS = \"$REPLICAS\"' > /tmp/haify-images.tpl && sudo -u oneadmin onedatastore create /tmp/haify-system.tpl && sudo -u oneadmin onedatastore create /tmp/haify-images.tpl"
	done
}

integrate_xcpng() {
	local n
	for n in "${NODES[@]}"; do
		if check_on "$n" "test -f /opt/xensource/sm/HaifySR.py" >/dev/null; then
			note "$n: SR driver installed"
			continue
		fi
		stage_dir "$n" "$DEPLOY_DIR/xcp-ng"
		run_on "$n" "cd $STAGING/xcp-ng && ./install.sh"
	done
	if check_on "$LOCAL_NODE" "xe sr-list type=haify --minimal | grep -q ." >/dev/null; then
		note "a Haify SR exists"
		return 0
	fi
	run_on "$LOCAL_NODE" "xe sr-create type=haify shared=true content-type=user name-label=Haify device-config:controller=$(q "$(controller_ip)") device-config:pool=$(q "$POOL") device-config:replicas=$(q "$REPLICAS")"
}

integrate_docker() {
	local n hosts=() args
	mapfile -t hosts < <(nodes_with docker)
	args="--controller $(controller_ip):3374 --pool $POOL"
	for n in "${hosts[@]}"; do
		if check_on "$n" "systemctl is-active --quiet haify-docker" >/dev/null; then
			note "$n: haify-docker running"
			continue
		fi
		copy_to "$n" "$BIN_DIR/haify-docker" /usr/local/bin/haify-docker 0755
		copy_to "$n" "$DEPLOY_DIR/docker/haify-docker.service" /etc/systemd/system/haify-docker.service 0644
		run_on "$n" "printf 'HAIFY_DOCKER_ARGS=\"%s\"\\n' $(q "$args") > /etc/default/haify-docker"
		# Podman asks the plugin with a 5 s timeout by default; creating a
		# replicated volume takes longer.
		run_on "$n" "if command -v podman >/dev/null; then mkdir -p /etc/containers/containers.conf.d && printf '%s\\n' '[engine]' 'volume_plugin_timeout = 120' '[engine.volume_plugins]' 'haify = \"/run/docker/plugins/haify.sock\"' > /etc/containers/containers.conf.d/haify.conf; fi"
		run_on "$n" "systemctl daemon-reload && systemctl enable --now haify-docker"
	done
	note "volumes: docker volume create -d haify --name data -o size=10G"
}

# integrate_kubernetes writes the chart values. The chart needs the haify-csi
# image in a registry the nodes pull from; with HAIFY_CSI_IMAGE set, and helm
# and kubectl working here, it is installed too.
integrate_kubernetes() {
	local out="$OUT_DIR/kubernetes" image="${HAIFY_CSI_IMAGE:-}"
	run_local mkdir -p "$out"
	write_local "$out/values.yaml" "$(cat <<EOF
controller:
  address: "$(controller_ip)"
storageClasses:
  - name: haify
    default: true
    pool: "$POOL"
    replicas: $REPLICAS
    reclaimPolicy: Delete
    volumeBindingMode: WaitForFirstConsumer
    allowVolumeExpansion: true
    parameters: {}
    annotations: {}
${image:+image:
  repository: "${image%:*}"
  tag: "${image##*:}"
}
EOF
)"
	if [ -n "$image" ] && command -v helm >/dev/null && command -v kubectl >/dev/null; then
		run_local helm upgrade --install haify-csi "$DEPLOY_DIR/helm/haify-csi" -n kube-system -f "$out/values.yaml"
		return 0
	fi
	note "wrote $out/values.yaml. Kubernetes node names must equal the Haify node names. Then:"
	note "  docker build -f Dockerfile.csi -t <registry>/haify-csi:<tag> .   # in the Haify source tree; push it"
	note "  helm install haify-csi $DEPLOY_DIR/helm/haify-csi -n kube-system -f $out/values.yaml \\"
	note "    --set image.repository=<registry>/haify-csi --set image.tag=<tag>"
}

integrate_nomad() {
	local out="$OUT_DIR/nomad" f
	run_local mkdir -p "$out"
	for f in haify-csi-controller.nomad.hcl haify-csi-node.nomad.hcl volume.hcl; do
		write_local "$out/$f" "$(sed "s/192\.168\.1\.10:3374/$(controller_ip):3374/g" "$DEPLOY_DIR/nomad/$f")"
	done
	note "wrote the jobs to $out with the controller filled in. Nomad node names must equal the Haify node names. Then:"
	note "  nomad job run $out/haify-csi-controller.nomad.hcl && nomad job run $out/haify-csi-node.nomad.hcl"
}

integrate_openstack() {
	local out="$OUT_DIR/openstack"
	integrate_libvirt
	run_local mkdir -p "$out"
	write_local "$out/cinder.conf.haify" "$(cat <<EOF
[DEFAULT]
enabled_backends = haify

[haify]
volume_driver = haify_cinder.driver.HaifyDriver
volume_backend_name = haify
haify_controller = $(controller_rest_list)
haify_pool = $POOL
EOF
)"
	note "on the cinder-volume host: pip install $DEPLOY_DIR/openstack, merge $out/cinder.conf.haify into cinder.conf,"
	note "  restart cinder-volume, then: openstack volume type create haify --property volume_backend_name=haify"
}

step_integration() {
	step 7 "connect $(env_title "$ENV")"
	case "$ENV" in
		libvirt) integrate_libvirt ;;
		opennebula) integrate_opennebula ;;
		xcp-ng) integrate_xcpng ;;
		docker) integrate_docker ;;
		kubernetes) integrate_kubernetes ;;
		nomad) integrate_nomad ;;
		openstack) integrate_openstack ;;
		plain) note "nothing to connect: create resources with 'haify resource create', gateways with 'haify gateway', HA with 'haify ha create'" ;;
	esac
}

# run_local / write_local: changes on this machine, printed in a dry run.
run_local() {
	if [ "$DRY_RUN" = 1 ]; then
		printf '[dry-run] %-12s %s\n' "(here)" "$*"
		return 0
	fi
	"$@"
}

write_local() {
	if [ "$DRY_RUN" = 1 ]; then
		printf '[dry-run] %-12s write %s\n' "(here)" "$1"
		return 0
	fi
	printf '%s\n' "$2" > "$1"
}
