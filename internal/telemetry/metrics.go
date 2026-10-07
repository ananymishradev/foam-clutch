package telemetry

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

type Metrics struct {
	Submissions       atomic.Uint64
	PreflightFailures atomic.Uint64
	StageCacheHits    atomic.Uint64
	StageCacheMisses  atomic.Uint64
	Requeues          atomic.Uint64
}

func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		lines := []string{
			"# TYPE foam_clutch_submissions_total counter",
			"foam_clutch_submissions_total " + strconv.FormatUint(m.Submissions.Load(), 10),
			"# TYPE foam_clutch_preflight_failures_total counter",
			"foam_clutch_preflight_failures_total " + strconv.FormatUint(m.PreflightFailures.Load(), 10),
			"# TYPE foam_clutch_stage_cache_hits_total counter",
			"foam_clutch_stage_cache_hits_total " + strconv.FormatUint(m.StageCacheHits.Load(), 10),
			"# TYPE foam_clutch_stage_cache_misses_total counter",
			"foam_clutch_stage_cache_misses_total " + strconv.FormatUint(m.StageCacheMisses.Load(), 10),
			"# TYPE foam_clutch_requeues_total counter",
			"foam_clutch_requeues_total " + strconv.FormatUint(m.Requeues.Load(), 10),
		}
		_, _ = fmt.Fprintln(w, strings.Join(lines, "\n"))
	})
}
