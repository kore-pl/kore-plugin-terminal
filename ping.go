package main

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// statusResult 是 kore.ping 的结果（PLUGIN-PROTOCOL §7.1）。
type statusResult struct {
	Status     string `json:"status"`
	UptimeS    int64  `json:"uptime_s"`
	Goroutines int    `json:"goroutines"`
	RSSKB      int64  `json:"rss_kb"`
}

// pingResult 组装 kore.ping 的存活与运行期指标。
func pingResult(startedAt time.Time) statusResult {
	return statusResult{
		Status:     "ready",
		UptimeS:    int64(time.Since(startedAt).Seconds()),
		Goroutines: runtime.NumGoroutine(),
		RSSKB:      rssKB(),
	}
}

// rssKB 读取 /proc/self/statm 的常驻页并换算为 KiB；非 Linux 或失败返回 0。
func rssKB() int64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize()) / 1024
}
