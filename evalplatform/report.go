package evalplatform

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
)

// ReportFile is the versioned snapshot written next to the HTML report.
type ReportFile struct {
	Version int     `json:"version"`
	Run     RunFile `json:"run"`
}

// WriteReport renders the frozen run. It does not reread the live dataset.
func WriteReport(runDir, htmlPath string) error {
	run, err := ReadRun(runDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(htmlPath), 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	body := renderHTML(run)
	if err := os.WriteFile(htmlPath, []byte(body), 0o644); err != nil {
		return err
	}
	snapshot := ReportFile{Version: runFileVersion, Run: run}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	jsonPath := strings.TrimSuffix(htmlPath, filepath.Ext(htmlPath)) + ".json"
	return os.WriteFile(jsonPath, append(data, '\n'), 0o644)
}

func renderHTML(run RunFile) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>")
	b.WriteString(html.EscapeString(run.Name))
	b.WriteString("</title></head><body><h1>")
	fmt.Fprintf(&b, "eval report version %d", run.Version)
	b.WriteString("</h1><p>started=")
	fmt.Fprintf(&b, "%t", run.Started)
	b.WriteString("</p>")
	for _, item := range run.Cases {
		b.WriteString("<article data-case=\"")
		b.WriteString(html.EscapeString(item.Case.ID))
		b.WriteString("\"><h2>")
		b.WriteString(html.EscapeString(item.Case.ID))
		b.WriteString("</h2><p class=\"execution\">")
		b.WriteString(html.EscapeString(item.Execution))
		b.WriteString("</p><p class=\"verdict\">")
		b.WriteString(html.EscapeString(item.Verdict))
		b.WriteString("</p>")
		for _, step := range item.Case.Steps() {
			b.WriteString("<p class=\"input\">")
			b.WriteString(html.EscapeString(step.Input.User))
			b.WriteString("</p>")
		}
		b.WriteString("</article>")
	}
	b.WriteString("</body></html>")
	return b.String()
}
