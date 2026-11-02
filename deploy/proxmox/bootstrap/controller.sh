# shellcheck shell=bash
# Step 3 (the dispatch SSH config) and step 4 (installing the controller and
# writing controller.toml), on every node that can run the controller.

DISPATCH_CONFIG=/root/.dispatch/config.toml
CONTROLLER_TOML=/etc/haify/controller.toml

# dispatch_key_path: the root key PVE created on the controller node. dispatch
# is given the path explicitly because Self-HA checks that the same path is
# readable on every standby before it hands the controller over.
dispatch_key_path() {
	local k
	for k in /root/.ssh/id_rsa /root/.ssh/id_ed25519 /root/.ssh/id_ecdsa; do
		if check_on "$CONTROLLER_NODE" "test -r $(q "$k")" >/dev/null 2>&1; then
			printf '%s' "$k"
			return 0
		fi
	done
	die "$CONTROLLER_NODE has no root SSH key in /root/.ssh (id_rsa, id_ed25519, id_ecdsa); PVE creates one when a node joins a cluster"
}

# write_dispatch_config <file> <key>: one section per node keyed by its
# address. The controller asks dispatch for hosts by address, so a section
# named after the node would never match and settings would fall through to
# ~/.ssh/config (docs/deployment-guide.md, "SSH trust and dispatch config").
write_dispatch_config() {
	local file="$1" key="$2" n ip
	{
		printf '# How haify-controller reaches the nodes over SSH (dispatch library).\n'
		printf '# Written by the Haify installer. Hosts are keyed by the address each node\n'
		printf '# is registered with in haify.\n\n'
		printf '[ssh]\nuser = "root"\nport = 22\nkey_path = "%s"\n' "$key"
		# dispatch records host keys in root'"'"'s known_hosts and adds an
		# unknown one on first contact, as the controller always has.
		printf 'known_hosts = "/root/.ssh/known_hosts"\nstrict_host_key = false\ntimeout = "30s"\n\n'
		printf '[exec]\nparallel = 10\n'
		for n in "${NODES[@]}"; do
			ip="${NODE_IP[$n]}"
			printf '\n# %s\n[hosts."%s"]\naddresses = ["%s"]\nuser = "root"\nkey_path = "%s"\n' "$n" "$ip" "$ip" "$key"
		done
	} > "$file"
}

step_dispatch() {
	step 3 "dispatch SSH config for the controller (${CONTROLLER_NODES[*]})"
	local key file want have n
	key=$(dispatch_key_path)
	file="$WORK_DIR/dispatch-config.toml"
	write_dispatch_config "$file" "$key"
	want=$(local_sha256 "$file")
	for n in "${CONTROLLER_NODES[@]}"; do
		check_on "$n" "test -r $(q "$key")" >/dev/null ||
			die "$n has no $key; a controller started there could not reach the nodes"
		have=$(remote_sha256 "$n" "$DISPATCH_CONFIG")
		if [ "$have" = "$want" ]; then
			note "$n: $DISPATCH_CONFIG up to date"
		elif [ -n "$have" ]; then
			# Somebody else's SSH settings for the controller: keep them. Self-HA
			# copies the active node's file to the standbys anyway.
			warn "$n: $DISPATCH_CONFIG exists and differs from what bootstrap would write; keeping it."
			warn "  It must reach every node as root by address, with $key (docs/deployment-guide.md, section 3)."
		else
			run_on "$n" "mkdir -p /root/.dispatch && chmod 700 /root/.dispatch"
			copy_to "$n" "$file" "$DISPATCH_CONFIG" 0600
		fi
	done
}

# toml_set <file> <section> <key> <value>: set one key in one section of a
# TOML file in place, adding the key (or the section) when it is absent.
# <value> is TOML, e.g. '"0.0.0.0"'.
toml_set() {
	local file="$1" tmp="$1.tmp"
	# Blank lines are held back so a key added to the end of a section lands
	# above the blank line that separates it from the next one.
	awk -v sec="[$2]" -v key="$3" -v val="$4" '
		function flush() { printf "%s", held; held = "" }
		/^[[:space:]]*$/ { held = held $0 "\n"; next }
		/^\[/ {
			if (insec && !done) { print key " = " val; done = 1 }
			flush()
			insec = ($0 == sec)
			print
			next
		}
		insec && !done && $0 ~ ("^[[:space:]]*" key "[[:space:]]*=") { flush(); print key " = " val; done = 1; next }
		{ flush(); print }
		END {
			if (!done) {
				if (insec) {
					print key " = " val
				} else {
					flush()
					print "\n" sec "\n" key " = " val
				}
			}
			flush()
		}
	' "$file" > "$tmp" && mv "$tmp" "$file"
}

# write_controller_toml <file>: the example config with only what bootstrap
# depends on pinned. Everything else (the database path among it) keeps the
# example's defaults.
write_controller_toml() {
	local file="$1" example="$CONFIG_DIR/controller.toml.example"
	[ -r "$example" ] || die "$example not found; set HAIFY_CONFIG_DIR to the directory holding controller.toml.example"
	cp "$example" "$file"
	# Every PVE node's storage plugin talks to the controller's REST port, and
	# under Self-HA the VIP moves between nodes: listen on every address.
	toml_set "$file" server listen_address '"0.0.0.0"'
	# The file step 3 wrote. Set explicitly because the controller runs from
	# systemd, where dispatch's own ~ lookup depends on the unit's HOME.
	toml_set "$file" dispatch config_path "\"$DISPATCH_CONFIG\""
}

# install_file_if_changed <node> <local> <remote> <mode>: copy only when the
# node's file differs, and record the path in INSTALLED when it did.
INSTALLED=""
install_file_if_changed() {
	local node="$1" src="$2" dest="$3" mode="$4"
	[ -r "$src" ] || die "$src not found"
	if [ "$(remote_sha256 "$node" "$dest")" = "$(local_sha256 "$src")" ]; then
		return 0
	fi
	copy_to "$node" "$src" "$dest" "$mode"
	INSTALLED+=" $dest"
}

install_from_binaries() {
	local node="$1" units
	INSTALLED=""
	if ! check_on "$node" "test -d /etc/haify && test -d /var/lib/haify && test -d /var/log/haify" >/dev/null; then
		run_on "$node" "mkdir -p /etc/haify /var/lib/haify /var/log/haify"
	fi
	install_file_if_changed "$node" "$CONFIG_DIR/haify-controller.service" /etc/systemd/system/haify-controller.service 0644
	install_file_if_changed "$node" "$CONFIG_DIR/service-ip@.service" /etc/systemd/system/service-ip@.service 0644
	units="$INSTALLED"
	install_file_if_changed "$node" "$BIN_DIR/haify-controller" /opt/haify/bin/haify-controller 0755
	install_file_if_changed "$node" "$BIN_DIR/service-ip" /opt/haify/bin/service-ip 0755
	install_file_if_changed "$node" "$BIN_DIR/service-ip" /usr/local/bin/service-ip 0755
	install_file_if_changed "$node" "$BIN_DIR/haify" /usr/local/bin/haify 0755
	if ! check_on "$node" "test -L /usr/local/bin/haify-cli" >/dev/null; then
		# Older scripts call the CLI by its former name.
		run_on "$node" "ln -sf haify /usr/local/bin/haify-cli"
	fi
	if [ -n "$units" ]; then
		run_on "$node" "systemctl daemon-reload"
	fi
	if [ -z "$INSTALLED" ]; then
		note "$node: haify binaries and units already up to date"
	elif check_on "$node" "systemctl is-active --quiet haify-controller" >/dev/null; then
		# Restarting here would be an unplanned failover under Self-HA; the
		# operator picks the moment (scripts/deploy.sh does it Self-HA aware).
		warn "$node: the running controller was updated on disk; restart it when convenient:"
		warn "  systemctl restart haify-controller   (under Self-HA, only on the active node)"
	fi
}

install_from_deb() {
	local node="$1"
	if deb_is_installed "$node" "$CONTROLLER_DEB"; then
		note "$node: $(deb_field "$CONTROLLER_DEB" Package) $(deb_field "$CONTROLLER_DEB" Version) already installed"
	else
		install_deb "$node" "$CONTROLLER_DEB"
	fi
	[ "$DRY_RUN" = 1 ] && return 0
	check_on "$node" "systemctl cat haify-controller >/dev/null 2>&1 && command -v haify >/dev/null" >/dev/null ||
		die "$node: after installing $CONTROLLER_DEB there is no haify-controller unit or no haify on PATH"
}

step_controller_install() {
	step 4 "install haify-controller and /etc/haify/controller.toml (${CONTROLLER_NODES[*]})"
	local n toml="$WORK_DIR/controller.toml"
	if [ -n "$CONTROLLER_DEB" ]; then
		log "installing from package $CONTROLLER_DEB"
	else
		log "installing binaries from $BIN_DIR, units from $CONFIG_DIR"
	fi
	write_controller_toml "$toml"
	for n in "${CONTROLLER_NODES[@]}"; do
		if [ -n "$CONTROLLER_DEB" ]; then
			install_from_deb "$n"
		else
			install_from_binaries "$n"
		fi
		if check_on "$n" "test -f $(q "$CONTROLLER_TOML")" >/dev/null; then
			# Never clobber a config: it may carry auth tokens or TLS settings,
			# and under Self-HA it is the active node's copy.
			note "$n: $CONTROLLER_TOML exists; left as is"
		else
			copy_to "$n" "$toml" "$CONTROLLER_TOML" 0644
		fi
	done
}

# check_artifacts verifies, before anything changes, that what step 4 will
# install is actually here.
check_artifacts() {
	if [ -n "$CONTROLLER_DEB" ]; then
		[ -r "$CONTROLLER_DEB" ] || die "HAIFY_CONTROLLER_DEB=$CONTROLLER_DEB is not readable"
		deb_field "$CONTROLLER_DEB" Package >/dev/null 2>&1 || die "$CONTROLLER_DEB is not a Debian package"
		return 0
	fi
	local f
	for f in haify-controller service-ip haify; do
		[ -x "$BIN_DIR/$f" ] || die "$BIN_DIR/$f not found or not executable.
Point HAIFY_BIN_DIR at linux binaries for the nodes (GOOS=linux make build puts them in bin/),
or HAIFY_CONTROLLER_DEB at an haify-controller_<version>_<arch>.deb."
	done
	for f in haify-controller.service service-ip@.service controller.toml.example; do
		[ -r "$CONFIG_DIR/$f" ] || die "$CONFIG_DIR/$f not found; set HAIFY_CONFIG_DIR to the repository's configs/ directory"
	done
}
