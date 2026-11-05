package server

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/yurika0211/luckyagent-runtime/logger"
)

const defaultMemoryGuardBytes int64 = 2 << 30

// processMemorySample is one scrape of process memory. ResidentBytes is 0
// when /proc/self/status is unavailable.
type processMemorySample struct {
	ResidentBytes int64
	HeapAlloc     uint64
	Sys           uint64
	Tripped       bool
}

var memoryGuardOnce sync.Once

func sampleProcessMemory() processMemorySample {
	return sampleProcessMemoryWithLimit(os.Getenv("GOMEMLIMIT"), 0)
}

func sampleProcessMemoryWithLimit(limitText string, thresholdOverride int64) processMemorySample {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	resident, ok := linuxResidentBytes()
	sample := processMemorySample{
		HeapAlloc: stats.HeapAlloc,
		Sys:       stats.Sys,
	}
	if ok {
		sample.ResidentBytes = resident
	}
	threshold := thresholdOverride
	if threshold <= 0 {
		threshold = memoryGuardThreshold(limitText)
	}
	used := int64(stats.HeapAlloc)
	if ok {
		used = resident
	}
	sample.Tripped = used >= threshold
	if sample.Tripped {
		memoryGuardOnce.Do(func() {
			logger.Warn("process memory guard tripped; serve keeps running",
				"resident_bytes", sample.ResidentBytes,
				"heap_alloc_bytes", sample.HeapAlloc,
				"sys_bytes", sample.Sys,
				"threshold_bytes", threshold,
			)
		})
	}
	return sample
}

func memoryGuardThreshold(limitText string) int64 {
	limit, ok := parseGoMemLimit(limitText)
	if !ok || limit <= 0 {
		return defaultMemoryGuardBytes
	}
	return limit
}

func parseGoMemLimit(text string) (int64, bool) {
	text = strings.TrimSpace(text)
	if text == "" || text == "off" {
		return 0, false
	}
	i := 0
	for i < len(text) && (text[i] == '+' || unicode.IsDigit(rune(text[i]))) {
		i++
	}
	if i == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(text[:i], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	unit := strings.TrimSpace(text[i:])
	mult := int64(1)
	switch strings.ToUpper(unit) {
	case "", "B":
		mult = 1
	case "K", "KB", "KIB":
		mult = 1 << 10
	case "M", "MB", "MIB":
		mult = 1 << 20
	case "G", "GB", "GIB":
		mult = 1 << 30
	case "T", "TB", "TIB":
		mult = 1 << 40
	default:
		return 0, false
	}
	if n > (1<<62)/mult {
		return 0, false
	}
	return n * mult, true
}

func linuxResidentBytes() (int64, bool) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb < 0 {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

func appendProcessMemoryMetrics(body string) string {
	sample := sampleProcessMemory()
	var b strings.Builder
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") && body != "" {
		b.WriteByte('\n')
	}
	if sample.ResidentBytes > 0 || linuxStatusPresent() {
		writeGauge(&b, "lh_process_resident_memory_bytes", "Resident set size from VmRSS.", sample.ResidentBytes)
	}
	writeGauge(&b, "lh_go_memstats_heap_alloc_bytes", "runtime.MemStats HeapAlloc.", int64(sample.HeapAlloc))
	writeGauge(&b, "lh_go_memstats_sys_bytes", "runtime.MemStats Sys.", int64(sample.Sys))
	tripped := int64(0)
	if sample.Tripped {
		tripped = 1
	}
	writeGauge(&b, "lh_process_memory_guard_tripped", "1 when RSS or heap is at the memory guard.", tripped)
	return b.String()
}

func linuxStatusPresent() bool {
	_, err := os.Stat("/proc/self/status")
	return err == nil
}

func writeGauge(b *strings.Builder, name, help string, value int64) {
	b.WriteString("# HELP ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(help)
	b.WriteByte('\n')
	b.WriteString("# TYPE ")
	b.WriteString(name)
	b.WriteString(" gauge\n")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(strconv.FormatInt(value, 10))
	b.WriteByte('\n')
}
