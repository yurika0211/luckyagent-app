package websocket

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yurika0211/luckyagent-runtime/config"
	"github.com/yurika0211/luckyagent-runtime/gateway"
)

// configAgentRuntime is a stubAgentRuntime that also satisfies runtimeConfigProvider,
// which attachmentForLocalPath needs to resolve artifact roots.
type configAgentRuntime struct {
	stubAgentRuntime
	cfg *config.Manager
}

func (c *configAgentRuntime) Config() *config.Manager { return c.cfg }

func TestFileSendDisplayNameOverridesStoredName(t *testing.T) {
	home := t.TempDir()
	mgr, err := config.NewManagerWithDir(filepath.Join(home, ".luckyagent"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	dir := filepath.Join(mgr.HomeDir(), "workspace", "deliverables")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stored := filepath.Join(dir, "ab12cd.pdf")
	if err := os.WriteFile(stored, []byte("pdf-bytes"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	h := NewAgentHandler(&configAgentRuntime{cfg: mgr})
	raw := `{"file_name":"季度报告.pdf","mime_type":"application/pdf","file_size":9,"file_path":"` + stored + `","file_url":"/api/v1/artifacts?path=workspace/deliverables/ab12cd.pdf"}`

	got := h.attachmentsFromToolResult("file_send", raw)
	if len(got) != 1 {
		t.Fatalf("attachments: %+v", got)
	}
	if got[0].FileName != "季度报告.pdf" {
		t.Fatalf("file name: got %q, want the display name rather than the stored hex name", got[0].FileName)
	}
	if got[0].FilePath != stored {
		t.Fatalf("file path changed: %s", got[0].FilePath)
	}
	if got[0].Type != gateway.AttachmentDocument {
		t.Fatalf("type: %s", got[0].Type)
	}
}

func TestFileSendDisplayNameDropsTraversal(t *testing.T) {
	if got := displayFileName("../secret"); got != "secret" {
		t.Fatalf("traversal kept: %q", got)
	}
	if got := displayFileName(".."); got != "" {
		t.Fatalf("bare .. kept: %q", got)
	}
	if got := displayFileName(""); got != "" {
		t.Fatalf("empty kept: %q", got)
	}
}
