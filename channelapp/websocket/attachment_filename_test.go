package websocket

import (
	"os"
	"path/filepath"
	"strings"
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

func TestAttachmentForLocalPathRejectsSymlinkOutsideCwd(t *testing.T) {
	home := t.TempDir()
	mgr, err := config.NewManagerWithDir(filepath.Join(home, ".luckyagent"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	link := filepath.Join(work, "escape.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	h := NewAgentHandler(&configAgentRuntime{cfg: mgr})
	if _, ok := h.attachmentForLocalPath(link, gateway.AttachmentDocument); ok {
		t.Fatal("symlink outside cwd became an attachment card")
	}
	raw := "MEDIA:" + link
	_, attachments := h.attachmentsFromResponse(raw)
	for _, attachment := range attachments {
		if strings.Contains(attachment.FileURL, "cwd") {
			t.Fatalf("emitted cwd card for outside symlink: %+v", attachment)
		}
	}
}

func TestSafeCwdArtifactRootRejectsFilesystemAndHome(t *testing.T) {
	if safeCwdArtifactRoot("/") {
		t.Fatal("filesystem root accepted")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if safeCwdArtifactRoot(home) {
		t.Fatal("user home accepted")
	}
	if !safeCwdArtifactRoot(t.TempDir()) {
		t.Fatal("ordinary temp directory rejected")
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
