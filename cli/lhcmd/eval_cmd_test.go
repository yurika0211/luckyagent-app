package lhcmd

import "testing"

func TestEvalCommandsRegistered(t *testing.T) {
	root := newRootCmd()
	for _, path := range [][]string{
		{"eval", "create-run"},
		{"eval", "start"},
		{"eval", "report"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil || cmd.Name() != path[len(path)-1] {
			t.Fatalf("missing %v: %v", path, err)
		}
	}
}
