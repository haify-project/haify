# shellcheck shell=bash
# shellcheck disable=SC2153 # the globals live in lib.sh and haify-install.sh
# --probe-json: what the web wizard (cmd/haify-installer) shows before anything
# is chosen: the nodes, whether root SSH reaches them, what they run and their
# disks. Read-only, and it never stops at a node it cannot reach: that node is
# reported, so the page can say how to fix it.

json_str() {
	local s="$1"
	s="${s//\\/\\\\}"
	s="${s//\"/\\\"}"
	s="${s//$'\t'/ }"
	s="${s//$'\n'/ }"
	printf '"%s"' "$s"
}

# json_list <items...>: a JSON array of strings.
json_list() {
	local out="" x
	for x in "$@"; do out+="${out:+,}$(json_str "$x")"; done
	printf '[%s]' "$out"
}

# probe_disks <node>: every whole disk with its size, model and verdict.
probe_disks() {
	local node="$1" path size model out="" verdict
	while read -r path size model; do
		[ -n "$path" ] || continue
		# ssh would read the rest of this loop's input; it gets none.
		verdict=$(device_verdict "$node" "$path" < /dev/null)
		out+="${out:+,}{\"path\":$(json_str "$path"),\"size\":$(json_str "$size"),\"model\":$(json_str "$model"),\"state\":$(json_str "$verdict")}"
	done < <(check_on "$node" "lsblk -dpno NAME,SIZE,TYPE,MODEL | awk '\$3 == \"disk\" && \$1 !~ /^\/dev\/(drbd|zram|nbd|rbd|loop)/ { m = \"\"; for (i = 4; i <= NF; i++) m = m \" \" \$i; print \$1, \$2, m }'" || true)
	printf '[%s]' "$out"
}

probe_json() {
	local n first=1 ssh envs pool
	printf '{"local":%s,"localEnv":%s,"nodes":[' "$(json_str "$LOCAL_NODE")" "$(json_str "$LOCAL_ENV")"
	for n in "${NODES[@]}"; do
		[ "$first" = 1 ] || printf ','
		first=0
		printf '{"name":%s,"ip":%s' "$(json_str "$n")" "$(json_str "${NODE_IP[$n]}")"
		if ! reachable "$n"; then
			printf ',"ssh":false}'
			continue
		fi
		detect_node "$n"
		ssh=true
		read -ra envs <<< "${NODE_ENVS[$n]}"
		pool=false
		pool_vg_present "$n" && pool=true
		printf ',"ssh":%s,"os":%s,"arch":%s,"envs":%s,"frontend":%s,"poolExists":%s,"disks":%s}' \
			"$ssh" "$(json_str "${NODE_OS[$n]}")" "$(json_str "${NODE_ARCH[$n]}")" "$(json_list "${envs[@]}")" \
			"$([ -n "${NODE_FRONTEND[$n]:-}" ] && echo true || echo false)" "$pool" "$(probe_disks "$n")"
	done
	printf ']}\n'
}
