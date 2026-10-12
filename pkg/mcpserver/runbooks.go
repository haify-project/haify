package mcpserver

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// The tools say what can be done; they cannot say in what order, or which
// call fails halfway if done first. Those live in runbooks, served two ways
// from the same text: as MCP prompts, which Claude Code offers as slash
// commands, and through haify_runbook, a read-only tool, because ChatGPT and
// claude.ai connectors take tools and ignore prompts, and the AI Copilot
// reads through tools alone.

//go:embed runbooks/*.md
var runbookFS embed.FS

type runbook struct {
	Name    string
	Title   string
	Summary string
	Needs   string
	Body    string
}

// loadRunbooks parses runbooks/*.md: a "# Title" line, a one-line summary, a
// "Needs: <role>" line, then the steps.
func loadRunbooks() ([]runbook, error) {
	log := zap.L()
	log.Debug("loadRunbooks: reading embedded runbooks")
	entries, err := runbookFS.ReadDir("runbooks")
	if err != nil {
		log.Debug("loadRunbooks: read dir failed", zap.Error(err))
		return nil, err
	}
	var out []runbook
	for _, e := range entries {
		raw, err := runbookFS.ReadFile("runbooks/" + e.Name())
		if err != nil {
			log.Debug("loadRunbooks: read file failed", zap.String("file", e.Name()), zap.Error(err))
			return nil, err
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		log.Debug("loadRunbooks: parsing runbook", zap.String("file", e.Name()), zap.Int("lines", len(lines)))
		if len(lines) < 4 || !strings.HasPrefix(lines[0], "# ") || !strings.HasPrefix(lines[2], "Needs: ") {
			log.Debug("loadRunbooks: runbook header malformed", zap.String("file", e.Name()))
			return nil, fmt.Errorf("runbooks/%s: want '# Title', a summary line, 'Needs: <role>', then the steps", e.Name())
		}
		out = append(out, runbook{
			Name:    strings.TrimSuffix(e.Name(), ".md"),
			Title:   strings.TrimPrefix(lines[0], "# "),
			Summary: strings.TrimSpace(lines[1]),
			Needs:   strings.TrimPrefix(lines[2], "Needs: "),
			Body:    strings.TrimSpace(strings.Join(lines, "\n")),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	log.Debug("loadRunbooks: loaded", zap.Int("count", len(out)))
	return out, nil
}

type runbookIn struct {
	Name string `json:"name,omitempty" jsonschema:"runbook to read; empty lists them all"`
}

type runbookOut struct {
	Runbooks []runbookInfo `json:"runbooks,omitempty"`
	Runbook  string        `json:"runbook,omitempty"`
}

type runbookInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Needs   string `json:"needs" jsonschema:"the token role the steps need: operate or admin"`
}

func (s *Server) registerRunbooks(srv *mcp.Server) {
	books, err := loadRunbooks()
	if err != nil {
		// Embedded at build time and covered by a test: not reachable in a
		// released binary, and a server without runbooks is still a server.
		s.logger.Error("runbooks not loaded")
		s.logger.Debug("Server.registerRunbooks: load failed", zap.Error(err))
		return
	}
	byName := make(map[string]runbook, len(books))
	infos := make([]runbookInfo, 0, len(books))
	for _, b := range books {
		s.logger.Debug("Server.registerRunbooks: registering runbook prompt", zap.String("runbook", b.Name),
			zap.String("needs", b.Needs))
		byName[b.Name] = b
		infos = append(infos, runbookInfo{Name: b.Name, Title: b.Title, Summary: b.Summary, Needs: b.Needs})

		srv.AddPrompt(&mcp.Prompt{
			Name:        b.Name,
			Title:       b.Title,
			Description: b.Summary + " (needs " + b.Needs + ")",
			Arguments: []*mcp.PromptArgument{{
				Name: "target", Description: "the resource or node this is for", Required: false,
			}},
		}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			text := b.Body
			target := strings.TrimSpace(req.Params.Arguments["target"])
			s.logger.Debug("runbookPrompt: serving runbook", zap.String("runbook", b.Name), zap.String("target", target))
			if target != "" {
				text += "\n\nApplies to: " + target
			}
			return &mcp.GetPromptResult{
				Description: b.Summary,
				Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}},
			}, nil
		})
	}

	addRead(s, srv, readOnlyTool("haify_runbook", "Read an operations runbook",
		"The order to do a multi-step operation in, and what fails halfway if it is done in another: switching the "+
			"controller or a gateway to another node, taking a node out of service, adding a replica, growing a "+
			"volume, checking and repairing replica data, failing a WAN resource back, renumbering nodes. Read the "+
			"runbook before starting such an operation. With no name it lists them."),
		func(_ context.Context, _ *mcp.CallToolRequest, in runbookIn) (*mcp.CallToolResult, runbookOut, error) {
			name := strings.TrimSpace(in.Name)
			s.logger.Debug("haify_runbook: reading runbook", zap.String("name", name))
			if name == "" {
				s.logger.Debug("haify_runbook: no name, listing runbooks", zap.Int("count", len(infos)))
				return nil, runbookOut{Runbooks: infos}, nil
			}
			b, ok := byName[name]
			if !ok {
				s.logger.Debug("haify_runbook: no such runbook", zap.String("name", name))
				names := make([]string, 0, len(byName))
				for n := range byName {
					names = append(names, n)
				}
				sort.Strings(names)
				return nil, runbookOut{}, fmt.Errorf("no runbook %q; there are %s", name, strings.Join(names, ", "))
			}
			s.logger.Debug("haify_runbook: found", zap.String("name", name), zap.Int("body_bytes", len(b.Body)))
			return nil, runbookOut{Runbook: b.Body}, nil
		})
}
