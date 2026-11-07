package server

import (
	"os"
	"testing"
)

func TestServeMemoryCapWarning(t *testing.T) {
	tests := []struct {
		name  string
		value string
		warn  bool
	}{
		{name: "unset", value: "", warn: true},
		{name: "blank", value: "   ", warn: true},
		{name: "set", value: "1536MiB", warn: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOMEMLIMIT", tt.value)
			got := serveMemoryCapWarning(os.Getenv("GOMEMLIMIT"))
			if tt.warn && got == "" {
				t.Fatal("expected a warning")
			}
			if !tt.warn && got != "" {
				t.Fatalf("warning = %q", got)
			}
		})
	}
}

func TestServeHasNoMemoryCapEnv(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	if serveMemoryCapWarning(os.Getenv("GOMEMLIMIT")) == "" {
		t.Fatal("empty GOMEMLIMIT should warn")
	}
	t.Setenv("GOMEMLIMIT", "1536MiB")
	if serveMemoryCapWarning(os.Getenv("GOMEMLIMIT")) != "" {
		t.Fatal("set GOMEMLIMIT should not warn")
	}
}
