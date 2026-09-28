package node

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metrics is a small Prometheus text-format registry (no dependency): HTTP
// request counts and latencies per route, plus gauges read on scrape.
type metrics struct {
	mu       sync.Mutex
	requests map[reqKey]uint64
	hist     map[string]*histogram
	started  time.Time
}

type reqKey struct {
	method, route string
	code          int
}

var latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

type histogram struct {
	counts []uint64 // per bucket, cumulative on output
	sum    float64
	n      uint64
}

func newMetrics() *metrics {
	return &metrics{requests: map[reqKey]uint64{}, hist: map[string]*histogram{}, started: time.Now()}
}

func (m *metrics) observe(method, route string, code int, d time.Duration) {
	if m == nil {
		return
	}
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		method = "other"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[reqKey{method, route, code}]++
	h := m.hist[route]
	if h == nil {
		h = &histogram{counts: make([]uint64, len(latencyBuckets))}
		m.hist[route] = h
	}
	s := d.Seconds()
	for i, b := range latencyBuckets {
		if s <= b {
			h.counts[i]++
			break
		}
	}
	h.sum += s
	h.n++
}

// handleMetrics serves Prometheus metrics when OBX_METRICS is on. With
// OBX_METRICS_TOKEN set, scrapers must send "Authorization: Bearer <token>".
func (a *App) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !a.Cfg.Metrics {
		failP(w, "not_found", "Metrics are off (set OBX_METRICS=true).")
		return
	}
	if a.Cfg.MetricsToken != "" && !constantEq(bearer(r), a.Cfg.MetricsToken) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
		failP(w, "unauthorized", "Send the metrics token.")
		return
	}
	var b strings.Builder
	a.metrics.write(&b)
	a.writeGauges(r.Context(), &b)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}

func (m *metrics) write(b *strings.Builder) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]reqKey, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].route != keys[j].route {
			return keys[i].route < keys[j].route
		}
		if keys[i].method != keys[j].method {
			return keys[i].method < keys[j].method
		}
		return keys[i].code < keys[j].code
	})
	b.WriteString("# HELP obx_http_requests_total HTTP requests by method, route pattern and status code.\n# TYPE obx_http_requests_total counter\n")
	for _, k := range keys {
		fmt.Fprintf(b, "obx_http_requests_total{method=%q,route=%q,code=\"%d\"} %d\n", k.method, k.route, k.code, m.requests[k])
	}
	routes := make([]string, 0, len(m.hist))
	for r := range m.hist {
		routes = append(routes, r)
	}
	sort.Strings(routes)
	b.WriteString("# HELP obx_http_request_duration_seconds HTTP request latency by route pattern.\n# TYPE obx_http_request_duration_seconds histogram\n")
	for _, r := range routes {
		h := m.hist[r]
		var cum uint64
		for i, le := range latencyBuckets {
			cum += h.counts[i]
			fmt.Fprintf(b, "obx_http_request_duration_seconds_bucket{route=%q,le=\"%s\"} %d\n", r, strconv.FormatFloat(le, 'g', -1, 64), cum)
		}
		fmt.Fprintf(b, "obx_http_request_duration_seconds_bucket{route=%q,le=\"+Inf\"} %d\n", r, h.n)
		fmt.Fprintf(b, "obx_http_request_duration_seconds_sum{route=%q} %g\n", r, h.sum)
		fmt.Fprintf(b, "obx_http_request_duration_seconds_count{route=%q} %d\n", r, h.n)
	}
	fmt.Fprintf(b, "# HELP obx_uptime_seconds Seconds since the process started.\n# TYPE obx_uptime_seconds gauge\nobx_uptime_seconds %d\n", int(time.Since(m.started).Seconds()))
}

// writeGauges adds state read from the database at scrape time.
func (a *App) writeGauges(ctx context.Context, b *strings.Builder) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	fmt.Fprintf(b, "# HELP obx_build_info Build information.\n# TYPE obx_build_info gauge\nobx_build_info{version=%q,database=%q,go=%q} 1\n", Version, a.DB.Dialect, runtime.Version())
	b.WriteString("# HELP obx_jobs Background jobs by kind and status.\n# TYPE obx_jobs gauge\n")
	if rs, err := a.DB.Query(ctx, `SELECT kind, status, COUNT(*) FROM jobs WHERE status IN ('queued', 'running', 'failed') GROUP BY kind, status ORDER BY kind, status`); err == nil {
		for rs.Next() {
			var kind, status string
			var n int
			if rs.Scan(&kind, &status, &n) == nil {
				fmt.Fprintf(b, "obx_jobs{kind=%q,status=%q} %d\n", kind, status, n)
			}
		}
		rs.Close()
	}
	one := func(name, help, q string) {
		var n int64
		if err := a.DB.QueryRow(ctx, q).Scan(&n); err == nil {
			fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, n)
		}
	}
	one("obx_webhook_deliveries_pending", "Webhook deliveries waiting to be sent or retried.", `SELECT COUNT(*) FROM webhook_deliveries WHERE status = 'pending'`)
	one("obx_mail_outbox_pending", "E-mails waiting for a retry.", `SELECT COUNT(*) FROM mail_outbox WHERE status = 'pending'`)
	one("obx_changes_latest_seq", "Latest change feed sequence number.", `SELECT COALESCE(MAX(seq), 0) FROM changes`)
	one("obx_renditions_failed", "Originals whose web versions could not be made.", `SELECT COUNT(*) FROM render_errors`)
	b.WriteString("# HELP obx_catalog_visible Published objects visible to retailers.\n# TYPE obx_catalog_visible gauge\n")
	for _, t := range []string{"brands", "products", "variants", "assets"} {
		var n int64
		if err := a.DB.QueryRow(ctx, `SELECT COUNT(*) FROM `+t+` WHERE visible = 1`).Scan(&n); err == nil {
			fmt.Fprintf(b, "obx_catalog_visible{type=%q} %d\n", strings.TrimSuffix(t, "s"), n)
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(b, "# HELP go_goroutines Number of goroutines.\n# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(b, "# HELP go_memstats_heap_alloc_bytes Heap bytes in use.\n# TYPE go_memstats_heap_alloc_bytes gauge\ngo_memstats_heap_alloc_bytes %d\n", ms.HeapAlloc)
	fmt.Fprintf(b, "# HELP go_memstats_sys_bytes Bytes obtained from the OS.\n# TYPE go_memstats_sys_bytes gauge\ngo_memstats_sys_bytes %d\n", ms.Sys)
}
