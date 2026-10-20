package opennebula

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Template is an OpenNebula template section (a datastore's or a disk's):
// its child elements by name.
type Template struct {
	Items []struct {
		XMLName xml.Name
		Value   string `xml:",chardata"`
	} `xml:",any"`
}

// Get returns the value of attribute name, trimmed, or "".
func (t Template) Get(name string) string {
	for _, it := range t.Items {
		if it.XMLName.Local == name {
			return strings.TrimSpace(it.Value)
		}
	}
	return ""
}

// Datastore is the part of a datastore's XML the drivers read.
type Datastore struct {
	ID       int      `xml:"ID"`
	Name     string   `xml:"NAME"`
	BasePath string   `xml:"BASE_PATH"`
	Template Template `xml:"TEMPLATE"`
}

// Image is the part of an image's XML the datastore driver reads.
type Image struct {
	ID              int    `xml:"ID"`
	Source          string `xml:"SOURCE"`
	Path            string `xml:"PATH"`
	Size            int64  `xml:"SIZE"` // MiB
	FS              string `xml:"FS"`
	CloningID       int    `xml:"CLONING_ID"`
	TargetSnapshot  string `xml:"TARGET_SNAPSHOT"`
	SnapshotIDs     []int  `xml:"SNAPSHOTS>SNAPSHOT>ID"`
	PersistentImage string `xml:"PERSISTENT"`
}

// DSAction is what OpenNebula hands a datastore driver action on stdin.
type DSAction struct {
	XMLName   xml.Name  `xml:"DS_DRIVER_ACTION_DATA"`
	Image     Image     `xml:"IMAGE"`
	Datastore Datastore `xml:"DATASTORE"`
}

// Disk is one DISK of a VM template.
type Disk struct {
	ID          int    `xml:"DISK_ID"`
	ImageID     string `xml:"IMAGE_ID"`
	Source      string `xml:"SOURCE"`
	Persistent  string `xml:"PERSISTENT"`
	Clone       string `xml:"CLONE"`
	TMMad       string `xml:"TM_MAD"`
	DatastoreID string `xml:"DATASTORE_ID"`
	Size        int64  `xml:"SIZE"` // MiB
	Type        string `xml:"TYPE"`
}

// VM is the part of a VM's XML the transfer driver reads.
type VM struct {
	XMLName  xml.Name `xml:"VM"`
	ID       int      `xml:"ID"`
	LCMState int      `xml:"LCM_STATE"`
	Disks    []Disk   `xml:"TEMPLATE>DISK"`
}

// Disk returns the VM's disk id, or nil.
func (v *VM) Disk(id int) *Disk {
	for i := range v.Disks {
		if v.Disks[i].ID == id {
			return &v.Disks[i]
		}
	}
	return nil
}

// LCM states (OpenNebula's VirtualMachine::LcmState) the transfer driver
// tells apart.
const (
	lcmPrologResume             = 9
	lcmHotplug                  = 17
	lcmEpilogStop               = 10
	lcmPrologUndeploy           = 31
	lcmEpilogUndeploy           = 30
	lcmEpilogStopFailure        = 41
	lcmEpilogUndeployFailure    = 42
	lcmPrologResumeFailure      = 49
	lcmPrologUndeployFailure    = 50
	lcmPrologMigrateUnknown     = 60
	lcmPrologMigrateUnknownFail = 61
)

// leavingHost is true in the states where mv takes a VM's disks off a host
// to the frontend (stop, undeploy): there is no destination host to set up.
func leavingHost(lcm int) bool {
	switch lcm {
	case lcmEpilogStop, lcmEpilogStopFailure, lcmEpilogUndeploy, lcmEpilogUndeployFailure:
		return true
	}
	return false
}

// sourceGone is true in the states where mv's source is the frontend or a
// host that failed: nothing on it is cleaned up.
func sourceGone(lcm int) bool {
	switch lcm {
	case lcmPrologResume, lcmPrologResumeFailure, lcmPrologUndeploy, lcmPrologUndeployFailure,
		lcmPrologMigrateUnknown, lcmPrologMigrateUnknownFail:
		return true
	}
	return false
}

// hostFailed is true when the VM is being moved off a host that failed
// (host_error's resched): the system datastore's mv cannot copy the VM
// directory, so the disks' links are made on the new host directly.
func hostFailed(lcm int) bool {
	return lcm == lcmPrologMigrateUnknown || lcm == lcmPrologMigrateUnknownFail
}

// hotplug is true while a disk is attached to or detached from a running
// guest. libvirt runs no hook for that, so the driver itself makes the disk
// Primary on the host before the attach and releases it after the detach.
func hotplug(lcm int) bool { return lcm == lcmHotplug }

func yes(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "YES") }

// Location is a transfer driver argument, host:path.
type Location struct {
	Host string
	Path string
}

// ParseLocation splits host:path at the first colon.
func ParseLocation(arg string) Location {
	host, p, ok := strings.Cut(arg, ":")
	if !ok {
		return Location{Path: arg}
	}
	return Location{Host: host, Path: p}
}

// DiskID is the disk number of a disk path (…/disk.3 → 3), or -1 when the
// path is the VM directory itself.
func (l Location) DiskID() int {
	base := path.Base(path.Clean(l.Path))
	n, ok := strings.CutPrefix(base, "disk.")
	if !ok {
		return -1
	}
	id, err := strconv.Atoi(n)
	if err != nil {
		return -1
	}
	return id
}

// Dir is the VM directory a disk path is in.
func (l Location) Dir() string { return path.Dir(path.Clean(l.Path)) }

// One runs OpenNebula's command-line tools on the frontend, as the driver's
// user (oneadmin), whose ONE_AUTH they read.
type One interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// ShowVM reads a VM's XML.
func ShowVM(ctx context.Context, one One, id int) (*VM, error) {
	log := zap.L()
	start := time.Now()
	out, err := one.Output(ctx, "onevm", "show", "-x", strconv.Itoa(id))
	log.Debug("ShowVM: onevm show returned", zap.Int("vm_id", id), zap.Int("bytes", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("onevm show %d: %w", id, err)
	}
	return ParseVM(out)
}

// ParseVM reads a VM's XML.
func ParseVM(data []byte) (*VM, error) {
	var vm VM
	if err := xml.Unmarshal(data, &vm); err != nil {
		zap.L().Debug("ParseVM: unreadable VM XML", zap.Int("bytes", len(data)), zap.Error(err))
		return nil, fmt.Errorf("reading the VM's XML: %w", err)
	}
	return &vm, nil
}

// ParseVM64 reads the base64 VM template the migrate actions get on stdin.
func ParseVM64(data []byte) (*VM, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		zap.L().Debug("ParseVM64: stdin is not base64", zap.Int("bytes", len(data)), zap.Error(err))
		return nil, fmt.Errorf("decoding the VM template on stdin: %w", err)
	}
	return ParseVM(raw)
}

// ShowDatastore reads a datastore's XML.
func ShowDatastore(ctx context.Context, one One, id string) (*Datastore, error) {
	log := zap.L()
	start := time.Now()
	out, err := one.Output(ctx, "onedatastore", "show", "-x", id)
	log.Debug("ShowDatastore: onedatastore show returned", zap.String("ds_id", id), zap.Int("bytes", len(out)),
		zap.Duration("took", time.Since(start)), zap.Error(err))
	if err != nil {
		return nil, fmt.Errorf("onedatastore show %s: %w", id, err)
	}
	var ds Datastore
	if err := xml.Unmarshal(out, &ds); err != nil {
		log.Debug("ShowDatastore: unreadable datastore XML", zap.String("ds_id", id), zap.Error(err))
		return nil, fmt.Errorf("reading datastore %s's XML: %w", id, err)
	}
	return &ds, nil
}

// ParseDSAction reads the base64 action data a datastore action gets on stdin.
func ParseDSAction(data []byte) (*DSAction, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		zap.L().Debug("ParseDSAction: stdin is not base64", zap.Int("bytes", len(data)), zap.Error(err))
		return nil, fmt.Errorf("decoding the driver action on stdin: %w", err)
	}
	var a DSAction
	if err := xml.Unmarshal(raw, &a); err != nil {
		zap.L().Debug("ParseDSAction: unreadable action XML", zap.Int("bytes", len(raw)), zap.Error(err))
		return nil, fmt.Errorf("reading the driver action XML: %w", err)
	}
	return &a, nil
}
