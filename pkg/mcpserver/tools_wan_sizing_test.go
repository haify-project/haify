package mcpserver

import "testing"

// Sizing reads and is there in every mode; tuning restarts tunnels and is a
// write, so a read-only server must not offer it.
func TestWANSizingAndTuningTools(t *testing.T) {
	tools := listTools(t, connect(t, fullClient{&mockClient{}}, false))
	for _, want := range []string{"haify_wan_sizing", "haify_wan_tune"} {
		if _, found := tools[want]; !found {
			t.Errorf("%s not registered", want)
		}
	}
	if tool := tools["haify_wan_sizing"]; tool != nil && (tool.Annotations == nil || !tool.Annotations.ReadOnlyHint) {
		t.Error("haify_wan_sizing must be marked read-only")
	}

	ro := listTools(t, connect(t, fullClient{&mockClient{}}, true))
	if _, found := ro["haify_wan_sizing"]; !found {
		t.Error("haify_wan_sizing must stay available in read-only mode")
	}
	if _, found := ro["haify_wan_tune"]; found {
		t.Error("haify_wan_tune must not be offered by a read-only server")
	}
}
