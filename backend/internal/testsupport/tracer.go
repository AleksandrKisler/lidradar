package testsupport

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// QueryStats собирает длительности запросов pgx по нормализованному тексту:
// профилирование PostgreSQL со стороны клиента без pg_stat_statements
// (LR-BE-2506). Включается переменной LIDRADAR_LOAD_TRACE.
type QueryStats struct {
	mutex   sync.Mutex
	samples map[string][]time.Duration
}

type QueryReport struct {
	SQL           string
	Count         int
	P50, P95, Max time.Duration
}

// LoadTrace — общий сборщик испытательных пулов.
var LoadTrace = &QueryStats{samples: make(map[string][]time.Duration)}

type traceKey struct{}

type traceEntry struct {
	sql     string
	started time.Time
}

// TraceEnabled сообщает, включено ли профилирование запросов.
func TraceEnabled() bool { return os.Getenv("LIDRADAR_LOAD_TRACE") != "" }

func (stats *QueryStats) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceKey{}, traceEntry{sql: data.SQL, started: time.Now()})
}

func (stats *QueryStats) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	entry, ok := ctx.Value(traceKey{}).(traceEntry)
	if !ok {
		return
	}
	key := normalizeSQL(entry.sql)
	elapsed := time.Since(entry.started)
	stats.mutex.Lock()
	stats.samples[key] = append(stats.samples[key], elapsed)
	stats.mutex.Unlock()
}

func normalizeSQL(sql string) string {
	collapsed := strings.Join(strings.Fields(sql), " ")
	if len(collapsed) > 110 {
		collapsed = collapsed[:110] + "…"
	}
	return collapsed
}

func (stats *QueryStats) Reset() {
	stats.mutex.Lock()
	defer stats.mutex.Unlock()
	stats.samples = make(map[string][]time.Duration)
}

// Overall возвращает перцентиль по всем запросам и их число.
func (stats *QueryStats) Overall(percentile float64) (time.Duration, int) {
	stats.mutex.Lock()
	defer stats.mutex.Unlock()
	all := make([]time.Duration, 0)
	for _, samples := range stats.samples {
		all = append(all, samples...)
	}
	return quantile(all, percentile), len(all)
}

// Top возвращает запросы с наибольшим p95.
func (stats *QueryStats) Top(limit int) []QueryReport {
	stats.mutex.Lock()
	defer stats.mutex.Unlock()
	reports := make([]QueryReport, 0, len(stats.samples))
	for sql, samples := range stats.samples {
		reports = append(reports, QueryReport{SQL: sql, Count: len(samples), P50: quantile(samples, 50), P95: quantile(samples, 95), Max: quantile(samples, 100)})
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].P95 > reports[j].P95 })
	if len(reports) > limit {
		reports = reports[:limit]
	}
	return reports
}

func quantile(samples []time.Duration, percentile float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(float64(len(sorted)-1) * percentile / 100)
	return sorted[index]
}
