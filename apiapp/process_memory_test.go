package server

import (
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestHandleMetricsIncludesProcessMemory(t *testing.T) {
	a := createTestAgent(t)
	s := New(a, DefaultServerConfig())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics", nil)
	w := httptest.NewRecorder()
	s.handleMetrics(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4" {
		t.Fatalf("content-type=%s", ct)
	}
	body := w.Body.String()
	heap := metricValue(t, body, "lh_go_memstats_heap_alloc_bytes")
	if heap <= 0 {
		t.Fatalf("heap=%d\n%s", heap, body)
	}
	tripped := metricValue(t, body, "lh_process_memory_guard_tripped")
	if tripped != 0 && tripped != 1 {
		t.Fatalf("tripped=%d", tripped)
	}
	if runtime.GOOS == "linux" {
		rss := metricValue(t, body, "lh_process_resident_memory_bytes")
		if rss <= 0 {
			t.Fatalf("rss=%d\n%s", rss, body)
		}
	}
	if !strings.Contains(body, "lh_go_memstats_sys_bytes") {
		t.Fatalf("missing sys gauge:\n%s", body)
	}
}

func TestProcessMemoryGuardTripsAtTinyThreshold(t *testing.T) {
	sample := sampleProcessMemoryWithLimit("", 1)
	if !sample.Tripped {
		t.Fatalf("expected trip, heap=%d rss=%d", sample.HeapAlloc, sample.ResidentBytes)
	}
	sample = sampleProcessMemoryWithLimit("2500MiB", 0)
	if sample.Tripped {
		t.Fatalf("2.5GiB limit should not trip a test process: %+v", sample)
	}
}

func TestParseGoMemLimit(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"2500MiB", 2500 << 20, true},
		{"2GiB", 2 << 30, true},
		{"off", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseGoMemLimit(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%q got %d ok=%v", tc.in, got, ok)
		}
	}
}

func metricValue(t *testing.T, body, name string) int64 {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") {
			n, err := strconv.ParseInt(strings.TrimPrefix(line, name+" "), 10, 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return n
		}
	}
	t.Fatalf("missing %s\n%s", name, body)
	return 0
}
