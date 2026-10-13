package mcpserver

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// fullClient offers every optional operation, so a server built on it
// registers every tool the real controller client would.
type fullClient struct{ *mockClient }

func (fullClient) RepairResource(context.Context, string) error { return nil }
func (fullClient) VerifyResource(context.Context, *haifypb.VerifyResourceRequest) (*haifypb.VerifyResourceResponse, error) {
	return &haifypb.VerifyResourceResponse{}, nil
}
func (fullClient) DRFailback(context.Context, string, string, uint32) (*haifypb.DRFailbackResponse, error) {
	return &haifypb.DRFailbackResponse{}, nil
}
func (fullClient) SetWanEndpoint(context.Context, *haifypb.SetWanEndpointRequest) (*haifypb.SetWanEndpointResponse, error) {
	return &haifypb.SetWanEndpointResponse{}, nil
}
func (fullClient) SetWanTuning(context.Context, *haifypb.SetWanTuningRequest) (*haifypb.SetWanTuningResponse, error) {
	return &haifypb.SetWanTuningResponse{}, nil
}
func (fullClient) GetWanSizing(context.Context, *haifypb.GetWanSizingRequest) (*haifypb.GetWanSizingResponse, error) {
	return &haifypb.GetWanSizingResponse{}, nil
}

func (fullClient) SetResourceProfileOptions(context.Context, string, map[string]string) (*haifypb.SetResourceProfileOptionsResponse, error) {
	return &haifypb.SetResourceProfileOptionsResponse{}, nil
}
func (fullClient) AdjustResourceProfile(context.Context, string, bool) (*haifypb.AdjustResourceProfileResponse, error) {
	return &haifypb.AdjustResourceProfileResponse{}, nil
}
func (fullClient) GetResourceProfileMaxSize(context.Context, string) (*haifypb.GetResourceProfileMaxSizeResponse, error) {
	return &haifypb.GetResourceProfileMaxSizeResponse{}, nil
}
func (fullClient) SetResourceProfile(context.Context, string, string) error { return nil }
func (fullClient) SetNodeAddress(context.Context, string, string, string) (*haifypb.SetNodeAddressResponse, error) {
	return &haifypb.SetNodeAddressResponse{}, nil
}
func (fullClient) SetNodeAddresses(context.Context, []*haifypb.NodeAddressMove) (*haifypb.SetNodeAddressResponse, error) {
	return &haifypb.SetNodeAddressResponse{}, nil
}

func (fullClient) ReplicationTLSStatus(context.Context, []string) ([]*haifypb.NodeTLSInfo, error) {
	return nil, nil
}
func (fullClient) SetResourceTLS(context.Context, string, bool) (string, error) { return "", nil }

func TestRunbooksParse(t *testing.T) {
	books, err := loadRunbooks()
	if err != nil {
		t.Fatal(err)
	}
	if len(books) < 7 {
		t.Fatalf("expected the runbooks to be embedded, got %d", len(books))
	}
	for _, b := range books {
		if b.Summary == "" || (b.Needs != "operate" && b.Needs != "admin") {
			t.Errorf("%s: summary %q, needs %q", b.Name, b.Summary, b.Needs)
		}
	}
}

// A runbook that names a tool the server does not have sends a caller looking
// for it; the runbooks and the tools have to move together.
func TestRunbooksOnlyNameToolsThatExist(t *testing.T) {
	session := connect(t, fullClient{&mockClient{}}, false)
	tools := listTools(t, session)
	books, _ := loadRunbooks()
	name := regexp.MustCompile(`\bhaify_[a-z0-9_]+\b`)
	for _, b := range books {
		for _, n := range name.FindAllString(b.Body, -1) {
			if _, ok := tools[n]; !ok {
				t.Errorf("runbook %s names %s, which is not a tool", b.Name, n)
			}
		}
	}
}

func TestRunbookToolAndPrompts(t *testing.T) {
	// Available to a read-only server: it is how a read token learns what to do.
	session := connect(t, &mockClient{}, true)
	if _, ok := listTools(t, session)["haify_runbook"]; !ok {
		t.Fatal("haify_runbook must exist on a read-only server")
	}
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_runbook"})
	if err != nil || res.IsError {
		t.Fatalf("list: %v %+v", err, res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{"reboot-node", "planned-switchover", "verify-and-repair"} {
		if !strings.Contains(text, want) {
			t.Errorf("listing lacks %s: %s", want, text)
		}
	}
	res, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_runbook", Arguments: map[string]any{"name": "planned-switchover"}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "haify_ha_evict") {
		t.Fatalf("get: %v %+v", err, res)
	}
	if res, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_runbook", Arguments: map[string]any{"name": "nope"}}); err == nil && !res.IsError {
		t.Fatal("an unknown runbook must be an error that lists the ones there are")
	}

	prompts, err := session.ListPrompts(t.Context(), nil)
	if err != nil || len(prompts.Prompts) < 7 {
		t.Fatalf("prompts: %v %d", err, len(prompts.Prompts))
	}
	got, err := session.GetPrompt(t.Context(), &mcp.GetPromptParams{Name: "reboot-node", Arguments: map[string]string{"target": "node-b"}})
	if err != nil {
		t.Fatal(err)
	}
	body := got.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(body, "haify_node_drain") || !strings.HasSuffix(body, "Applies to: node-b") {
		t.Fatalf("prompt: %s", body)
	}
}

type tiebreakingClient struct {
	*mockClient
	got [2]string
}

func (c *tiebreakingClient) SetTiebreaker(_ context.Context, resource, node string) (string, string, error) {
	c.got = [2]string{resource, node}
	// previous node first, message second — as the real client returns them
	return "sdt2", "", nil
}

// The tool must report the controller's message, not the previous node the
// client returns first, and an empty node removes the tiebreaker.
func TestTiebreakerToolRemovesOnEmptyNodeAndSaysSo(t *testing.T) {
	tc := &tiebreakingClient{mockClient: &mockClient{}}
	session := connect(t, tc, false)
	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "haify_resource_set_tiebreaker", Arguments: map[string]any{"resource": "r3c", "node": ""}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if tc.got != [2]string{"r3c", ""} {
		t.Fatalf("the empty node must reach the controller as a removal: %v", tc.got)
	}
	out := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(out, "removed") || strings.Contains(out, "sdt2") {
		t.Fatalf("message: %s", out)
	}
}
