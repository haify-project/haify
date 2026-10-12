package inspect

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// ProbeScript gathers everything node-level an inspection needs in one
// shell run, so a node costs one SSH round however many checks read it.
//
// Every line is key=value on a single line. Multi-line material (lvs,
// drbdsetup JSON, a certificate) is base64-wrapped so a stray newline or a
// quote in it cannot break the framing. Each command's stderr is dropped: the
// deployment client merges stderr into the output, and a warning from lvs
// must not be read as a key. The probe=1 / end=1 pair lets the parser tell a
// probe that ran to completion from one that was cut off.
//
// Run it as root (lvs and drbdsetup need it), base64-wrapped, so dispatch's
// sh -c quoting cannot empty its $variables.
const ProbeScript = `export LC_ALL=C
echo "probe=1"
echo "hostname=$(hostname 2>/dev/null)"
echo "arch=$(uname -m 2>/dev/null)"
echo "now=$(date +%s.%N 2>/dev/null)"
echo "ntp=$(timedatectl show -p NTPSynchronized --value 2>/dev/null)"
df -P / 2>/dev/null | awk 'NR==2 {gsub("%","",$5); print "rootfs="$5}'
ip -o addr show 2>/dev/null | awk '{split($4,a,"/"); print "addr="a[1]}'
if [ -r /sys/module/drbd/version ]; then echo "drbd_kmod=$(cat /sys/module/drbd/version 2>/dev/null)"
elif [ -r /proc/drbd ]; then echo "drbd_kmod=$(sed -n 's/^version: \([^ ]*\).*/\1/p' /proc/drbd 2>/dev/null | head -1)"; fi
echo "drbd_utils=$(drbdadm --version 2>/dev/null | sed -n 's/^DRBDADM_VERSION=//p' | head -1)"
echo "reactor=$(drbd-reactor --version 2>/dev/null | head -1 | awk '{print $NF}')"
echo "reactor_active=$(systemctl is-active drbd-reactor 2>/dev/null)"
echo "ctl_active=$(systemctl is-active haify-controller 2>/dev/null)"
bin=$(systemctl cat haify-controller 2>/dev/null | sed -n 's/^ExecStart=[-@+!]*\([^ ]*\).*/\1/p' | tail -1)
if [ -n "$bin" ] && [ -f "$bin" ]; then echo "ctl_bin=$bin $(sha256sum "$bin" 2>/dev/null | awk '{print $1}')"; echo "ctl_machine=$(od -An -tx1 -j18 -N2 "$bin" 2>/dev/null | tr -d ' \n')"; fi
for f in /etc/drbd-reactor.d/*.toml /etc/drbd-reactor.d/*.toml.disabled; do [ -f "$f" ] && echo "reactor_conf=$(basename "$f")"; done
for f in /etc/drbd.d/*.res; do [ -f "$f" ] && echo "res_file=$(basename "$f" .res)"; done
awk '!/^[[:space:]]*#/ && NF>=2 {l=$1; for(i=2;i<=NF;i++){if($i ~ /^#/) break; l=l" "$i}; print "hosts="l}' /etc/hosts 2>/dev/null
[ -s /etc/haify/drbd-tls/node.crt ] && echo "tls_cert=$(base64 -w0 /etc/haify/drbd-tls/node.crt 2>/dev/null)"
echo "lvs=$(lvs --noheadings --nosuffix --units b --separator '|' -o vg_name,lv_name,segtype,lv_size,data_percent,metadata_percent 2>/dev/null | base64 -w0)"
vgs --noheadings --nosuffix --units b --separator '|' -o vg_name,vg_free 2>/dev/null | awk -F'|' '{gsub(/ /,"",$1); gsub(/ /,"",$2); if ($1 != "") print "vg_free="$1" "$2}'
echo "drbd=$(drbdsetup status --json 2>/dev/null | base64 -w0)"
echo "end=1"
`

// HostsEntry is one /etc/hosts line.
type HostsEntry struct {
	IP    string
	Names []string
}

// LV is one logical volume as lvs reported it.
type LV struct {
	VG        string
	Name      string
	Segtype   string
	SizeBytes uint64
	// DataPercent and MetaPercent are only meaningful when HasUsage: lvs
	// leaves them blank for anything that is not an active thin pool.
	DataPercent float64
	MetaPercent float64
	HasUsage    bool
}

// NodeProbe is what one node reported.
type NodeProbe struct {
	// Complete is false when the output stopped before its end marker; the
	// keys that did arrive are still used.
	Complete  bool
	Hostname  string
	Arch      string  // uname -m
	Now       float64 // seconds since the epoch, 0 when not reported
	NTPSynced string  // "yes", "no", or "" when timedatectl is absent
	RootUse   int     // percent of / in use, -1 when not reported
	Addrs     []string
	DRBDKmod  string
	DRBDUtils string
	Reactor   string
	ReactorUp string
	CtlActive string
	CtlBin    string
	CtlSHA    string
	// CtlMachine is the ELF e_machine of the controller binary, as the two
	// little-endian bytes in hex ("3e00" is x86-64, "b700" AArch64).
	CtlMachine  string
	ReactorConf []string
	ResFiles    []string
	Hosts       []HostsEntry
	TLSCert     []byte
	LVs         []LV
	LVsOK       bool
	// VGFree is each volume group's free bytes.
	VGFree map[string]uint64
	DRBD   []DRBDResource
	DRBDOK bool
	// Problems are parse failures of individual sections, kept as evidence.
	Problems []string
}

// ParseProbe reads ProbeScript's output. It fails only when the output is not
// from the probe at all; a damaged section is recorded in Problems and the
// rest is kept, so one unreadable lvs does not blank a node's clock check.
func ParseProbe(output string) (*NodeProbe, error) {
	p := &NodeProbe{RootUse: -1}
	seen := false
	for _, raw := range strings.Split(output, "\n") {
		key, val, ok := strings.Cut(strings.TrimRight(raw, "\r"), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "probe":
			seen = true
		case "end":
			p.Complete = true
		case "hostname":
			p.Hostname = val
		case "now":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				p.Now = f
			}
		case "ntp":
			p.NTPSynced = val
		case "rootfs":
			if n, err := strconv.Atoi(val); err == nil {
				p.RootUse = n
			}
		case "addr":
			if val != "" {
				p.Addrs = append(p.Addrs, val)
			}
		case "drbd_kmod":
			p.DRBDKmod = val
		case "drbd_utils":
			p.DRBDUtils = val
		case "reactor":
			p.Reactor = val
		case "reactor_active":
			p.ReactorUp = val
		case "ctl_active":
			p.CtlActive = val
		case "ctl_bin":
			if path, sum, ok := strings.Cut(val, " "); ok {
				p.CtlBin, p.CtlSHA = path, strings.TrimSpace(sum)
			}
		case "reactor_conf":
			p.ReactorConf = append(p.ReactorConf, val)
		case "res_file":
			p.ResFiles = append(p.ResFiles, val)
		case "hosts":
			if f := strings.Fields(val); len(f) >= 2 {
				p.Hosts = append(p.Hosts, HostsEntry{IP: f[0], Names: f[1:]})
			}
		case "tls_cert":
			if b, err := base64.StdEncoding.DecodeString(val); err == nil {
				p.TLSCert = b
			}
		case "arch":
			p.Arch = val
		case "ctl_machine":
			p.CtlMachine = val
		case "vg_free":
			if vg, free, ok := strings.Cut(val, " "); ok {
				if n, err := strconv.ParseUint(strings.TrimSpace(free), 10, 64); err == nil {
					if p.VGFree == nil {
						p.VGFree = map[string]uint64{}
					}
					p.VGFree[vg] = n
				}
			}
		case "lvs":
			p.parseLVs(val)
		case "drbd":
			p.parseDRBD(val)
		}
	}
	if !seen {
		zap.L().Debug("ParseProbe: no probe marker in output", zap.Int("bytes", len(output)))
		return nil, fmt.Errorf("output is not from the inspection probe: %q", firstLine(output))
	}
	zap.L().Debug("ParseProbe: parsed", zap.String("hostname", p.Hostname), zap.Bool("complete", p.Complete),
		zap.String("arch", p.Arch), zap.Int("root_use", p.RootUse), zap.Int("addrs", len(p.Addrs)),
		zap.Int("lvs", len(p.LVs)), zap.Bool("lvs_ok", p.LVsOK), zap.Int("drbd_resources", len(p.DRBD)),
		zap.Bool("drbd_ok", p.DRBDOK), zap.Int("reactor_confs", len(p.ReactorConf)), zap.Bool("has_tls_cert", len(p.TLSCert) > 0),
		zap.Strings("problems", p.Problems))
	return p, nil
}

func (p *NodeProbe) parseLVs(b64 string) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		zap.L().Debug("NodeProbe.parseLVs: base64 decode failed", zap.String("hostname", p.Hostname), zap.Error(err))
		p.Problems = append(p.Problems, "lvs output unreadable: "+err.Error())
		return
	}
	p.LVsOK = true
	defer func() {
		zap.L().Debug("NodeProbe.parseLVs: parsed", zap.String("hostname", p.Hostname), zap.Int("count", len(p.LVs)))
	}()
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) < 6 || f[0] == "" {
			continue
		}
		lv := LV{VG: strings.TrimSpace(f[0]), Name: strings.TrimSpace(f[1]), Segtype: strings.TrimSpace(f[2])}
		lv.SizeBytes, _ = strconv.ParseUint(strings.TrimSpace(f[3]), 10, 64)
		d, derr := strconv.ParseFloat(strings.TrimSpace(f[4]), 64)
		m, merr := strconv.ParseFloat(strings.TrimSpace(f[5]), 64)
		if derr == nil && merr == nil {
			lv.DataPercent, lv.MetaPercent, lv.HasUsage = d, m, true
		}
		p.LVs = append(p.LVs, lv)
	}
}

func (p *NodeProbe) parseDRBD(b64 string) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		zap.L().Debug("NodeProbe.parseDRBD: base64 decode failed", zap.String("hostname", p.Hostname), zap.Error(err))
		p.Problems = append(p.Problems, "drbdsetup output unreadable: "+err.Error())
		return
	}
	res, err := ParseDRBDStatus(string(raw))
	if err != nil {
		zap.L().Debug("NodeProbe.parseDRBD: status does not parse", zap.String("hostname", p.Hostname), zap.Error(err))
		p.Problems = append(p.Problems, err.Error())
		return
	}
	zap.L().Debug("NodeProbe.parseDRBD: parsed", zap.String("hostname", p.Hostname), zap.Int("count", len(res)))
	p.DRBD, p.DRBDOK = res, true
}

// Resource returns the node's own view of a DRBD resource, or nil.
func (p *NodeProbe) Resource(name string) *DRBDResource {
	for i := range p.DRBD {
		if p.DRBD[i].Name == name {
			return &p.DRBD[i]
		}
	}
	return nil
}

// HasAddr reports whether addr is configured on one of the node's interfaces.
func (p *NodeProbe) HasAddr(addr string) bool {
	for _, a := range p.Addrs {
		if a == addr {
			return true
		}
	}
	return false
}

// HasReactorConf reports whether an enabled promoter config file exists.
func (p *NodeProbe) HasReactorConf(name string) bool {
	for _, f := range p.ReactorConf {
		if f == name {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
