package rbac

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		method string
		object string
		action string
	}{
		{"/v1.HaifyController/CreatePool", "pool", ActWrite},
		{"/v1.HaifyController/ListPools", "pool", ActRead},
		{"/v1.HaifyController/AddDiskToPool", "pool", ActWrite},
		{"/v1.HaifyController/CreateZFSSnapshot", "snapshot", ActWrite},
		{"/v1.HaifyController/ListSnapshots", "snapshot", ActRead},
		{"/v1.HaifyController/CreateNFSGateway", "gateway", ActWrite},
		{"/v1.HaifyController/AddNVMeNamespace", "gateway", ActWrite},
		{"/v1.HaifyController/ListGateways", "gateway", ActRead},
		{"/v1.HaifyController/SetSMBUser", "gateway", ActWrite},
		{"/v1.HaifyController/ListSMBShares", "gateway", ActRead},
		{"/v1.HaifyController/EnableSelfHa", "ha", ActWrite},
		{"/v1.HaifyController/GetSelfHaStatus", "ha", ActRead},
		{"/v1.HaifyController/RegisterNode", "node", ActWrite},
		{"/v1.HaifyController/ListNodes", "node", ActRead},
		{"/v1.HaifyController/CreateResource", "resource", ActWrite},
		// Rewrites and adjusts the config on every node: a write, not a read,
		// however much it sounds like maintenance.
		{"/v1.HaifyController/RepairResource", "resource", ActWrite},
		{"/v1.HaifyController/ResourceStatus", "resource", ActRead},
		{"/v1.HaifyController/SetPrimary", "resource", ActWrite},
		// Off-cluster backups must not fall through to "system", where an
		// operator has read only: the feature would then be unusable by
		// anyone but admin, and nothing would say so.
		{"/v1.HaifyController/CreateBackup", "backup", ActWrite},
		{"/v1.HaifyController/RestoreBackup", "backup", ActWrite},
		{"/v1.HaifyController/AddBackupTarget", "backup", ActWrite},
		{"/v1.HaifyController/ShipResource", "backup", ActWrite},
		{"/v1.HaifyController/PrepareShipReceive", "backup", ActWrite},
		{"/v1.HaifyController/ListBackups", "backup", ActRead},
		{"/v1.HaifyController/ListBackupTargets", "backup", ActRead},
		// Database applications are their own object; SnapshotApp is not a
		// "snapshot" and the approval methods are not apps.
		{"/v1.HaifyController/CreateApp", "app", ActWrite},
		{"/v1.HaifyController/ListApps", "app", ActRead},
		{"/v1.HaifyController/GetAppStatus", "app", ActRead},
		{"/v1.HaifyController/DeleteApp", "app", ActWrite},
		{"/v1.HaifyController/FailoverApp", "app", ActWrite},
		{"/v1.HaifyController/SnapshotApp", "app", ActWrite},
		{"/v1.HaifyController/ListApprovals", "approval", ActRead},
		{"/v1.HaifyController/ApproveRequest", "approval", ActApprove},
	}
	for _, c := range cases {
		obj, act := Classify(c.method)
		if obj != c.object || act != c.action {
			t.Errorf("Classify(%s) = (%s, %s), want (%s, %s)",
				c.method, obj, act, c.object, c.action)
		}
	}
}

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	users := []User{
		{Name: "alice", Token: "alice-token-0123456789", Role: "admin"},
		{Name: "olivia", Token: "olivia-token-0123456789", Role: "operator"},
		{Name: "victor", Token: "victor-token-0123456789", Role: "viewer"},
	}
	e, err := New(nil, users, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestEnforceRoles(t *testing.T) {
	e := newTestEngine(t)
	cases := []struct {
		user, object, action string
		want                 bool
	}{
		// admin can do anything
		{"alice", "pool", ActWrite, true},
		{"alice", "system", ActWrite, true},
		// operator manages storage but not system writes
		{"olivia", "gateway", ActWrite, true},
		{"olivia", "snapshot", ActWrite, true},
		{"olivia", "system", ActWrite, false},
		{"olivia", "node", ActWrite, false},
		{"olivia", "node", ActRead, true},
		// viewer is read-only
		{"victor", "pool", ActRead, true},
		{"victor", "pool", ActWrite, false},
		{"victor", "gateway", ActWrite, false},
		// unknown subject is denied
		{"nobody", "pool", ActRead, false},
	}
	for _, c := range cases {
		got, err := e.Enforce(c.user, c.object, c.action)
		if err != nil {
			t.Fatalf("Enforce(%s,%s,%s): %v", c.user, c.object, c.action, err)
		}
		if got != c.want {
			t.Errorf("Enforce(%s, %s, %s) = %v, want %v",
				c.user, c.object, c.action, got, c.want)
		}
	}
}

func TestResolveUser(t *testing.T) {
	e := newTestEngine(t)
	if name, ok := e.ResolveUser("alice-token-0123456789"); !ok || name != "alice" {
		t.Errorf("ResolveUser(valid) = (%q, %v), want (alice, true)", name, ok)
	}
	if _, ok := e.ResolveUser("wrong-token"); ok {
		t.Errorf("ResolveUser(invalid) should fail")
	}
	if _, ok := e.ResolveUser(""); ok {
		t.Errorf("ResolveUser(empty) should fail")
	}
}

func TestCustomPolicyAndRole(t *testing.T) {
	users := []User{{Name: "dan", Token: "dan-token-0123456789xx", Role: "deployer"}}
	policies := []Policy{
		{Role: "deployer", Object: "gateway", Action: ActWrite},
		{Role: "deployer", Object: "gateway", Action: ActRead},
	}
	e, err := New(nil, users, policies)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ok, _ := e.Enforce("dan", "gateway", ActWrite); !ok {
		t.Errorf("custom role should allow gateway write")
	}
	if ok, _ := e.Enforce("dan", "pool", ActWrite); ok {
		t.Errorf("custom role should not allow pool write")
	}
}

func TestInvalidInputs(t *testing.T) {
	if _, err := New(nil, []User{{Name: "", Token: "x", Role: "admin"}}, nil); err == nil {
		t.Errorf("expected error for user with empty name")
	}
	if _, err := New(nil, nil, []Policy{{Role: "admin", Object: "", Action: "read"}}); err == nil {
		t.Errorf("expected error for policy with empty object")
	}
}

// Running an inspection changes nothing on the cluster, so a read-only token
// may run one, exactly as it may list the reports.
func TestInspectionRPCsAreReads(t *testing.T) {
	for _, m := range []string{"RunInspection", "ListInspections", "GetInspection"} {
		object, action := Classify("/v1.HaifyController/" + m)
		if object != "system" || action != ActRead {
			t.Errorf("%s: got %s/%s, want system/%s", m, object, action, ActRead)
		}
	}
}

// Notification channels carry a signing secret and decide who gets paged, so
// they are administrative configuration: readable by an operator, writable only
// by an admin. That is what the unclassified default already gives them, and
// this pins it — a later object rule that happened to match "Channel" would
// otherwise widen who can redirect a cluster's alerts without anyone noticing.
func TestNotifyChannelRPCsAreSystemScoped(t *testing.T) {
	for _, tc := range []struct {
		method string
		action string
	}{
		{"ListNotifyChannels", ActRead},
		{"SaveNotifyChannel", ActWrite},
		{"DeleteNotifyChannel", ActWrite},
		{"TestNotifyChannel", ActWrite},
	} {
		object, action := Classify("/v1.HaifyController/" + tc.method)
		if object != "system" {
			t.Errorf("%s: object = %q, want system", tc.method, object)
		}
		if action != tc.action {
			t.Errorf("%s: action = %q, want %q", tc.method, action, tc.action)
		}
	}
}

// Deciding a held-back call is its own right: admin has it, security-officer
// has only it and reading, operator does not have it.
func TestApprovalRight(t *testing.T) {
	e, err := New(nil, []User{
		{Name: "sec", Token: "sec-token-0123456789", Role: "security-officer"},
		{Name: "op", Token: "op-token-0123456789", Role: "operator"},
		{Name: "adm", Token: "adm-token-0123456789", Role: "admin"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	obj, act := Classify("/v1.HaifyController/ApproveRequest")
	if obj != "approval" || act != ActApprove {
		t.Fatalf("ApproveRequest classified as %s:%s", obj, act)
	}
	for user, want := range map[string]bool{"sec": true, "adm": true, "op": false} {
		if got, _ := e.Enforce(user, obj, act); got != want {
			t.Errorf("%s approve = %v, want %v", user, got, want)
		}
	}
	if ok, _ := e.Enforce("sec", "backup", ActWrite); ok {
		t.Error("security-officer must not change anything itself")
	}
}
