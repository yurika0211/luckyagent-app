package lhcmd

import (
	"strings"
	"testing"

	"github.com/yurika0211/luckyagent-runtime/session"
)

func TestResolveSessionByIDPrefix(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("new session manager: %v", err)
	}
	mgr.Ensure("alpha-one")
	mgr.Ensure("beta-one")

	sess, err := resolveSessionByIDPrefix(mgr, "alpha")
	if err != nil {
		t.Fatalf("resolve unique prefix: %v", err)
	}
	if sess.ID != "alpha-one" {
		t.Fatalf("expected alpha-one, got %s", sess.ID)
	}

	if _, err := resolveSessionByIDPrefix(mgr, "missing"); err == nil {
		t.Fatal("expected missing prefix to fail")
	}
}

func TestResolveSessionByIDPrefixRejectsAmbiguousPrefix(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("new session manager: %v", err)
	}
	mgr.Ensure("alpha-one")
	mgr.Ensure("alpha-two")

	_, err = resolveSessionByIDPrefix(mgr, "alpha")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous prefix error, got %v", err)
	}
}

func TestREPLSessionCommandsRejectAmbiguousPrefix(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	current := mgr.Ensure("active-session")
	mgr.Ensure("alpha-one")
	mgr.Ensure("alpha-two")
	handleSessionCommand("switch alpha", nil, mgr, &current)
	if current.ID != "active-session" {
		t.Fatalf("ambiguous switch selected %s", current.ID)
	}
	handleSessionCommand("delete alpha", nil, mgr, &current)
	for _, id := range []string{"alpha-one", "alpha-two"} {
		if _, ok := mgr.Get(id); !ok {
			t.Fatalf("ambiguous delete removed %s", id)
		}
	}
}

func TestREPLSwitchDisplaysStoredRecapWithoutAgent(t *testing.T) {
	mgr, err := session.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	current := mgr.Ensure("current-session")
	target := mgr.Ensure("recap-session")
	target.SetTitle("Manual title")
	target.AddMessage("user", "question")
	target.AddMessage("assistant", "answer")
	if err := target.Save(); err != nil {
		t.Fatal(err)
	}
	_, _, version, err := target.RecapSnapshot(10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.UpdateRecap("你问了 question。", version); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		handleSessionCommand("switch recap", nil, mgr, &current)
		return nil
	})
	if err != nil || current != target || target.Title != "Manual title" || !strings.Contains(out, "◇ 你问了 question。") {
		t.Fatalf("switch output=%q error=%v", out, err)
	}
}
