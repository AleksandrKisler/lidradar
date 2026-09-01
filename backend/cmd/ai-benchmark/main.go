// Команда ai-benchmark проверяет размеченную выборку через совместимый с
// OpenAI маршрут llama.cpp и выдаёт машиночитаемый отчёт.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"lidradar/backend/internal/ai/benchmark"
	"lidradar/backend/internal/ai/infrastructure"
)

func main() {
	datasetPath := flag.String("dataset", "models/datasets/golden_v1.jsonl", "путь к версионированной JSONL-выборке")
	digestPath := flag.String("checksum", "models/datasets/golden_v1.sha256", "путь к утверждённой контрольной сумме")
	endpoint := flag.String("endpoint", "http://127.0.0.1:8080/v1/chat/completions", "маршрут llama.cpp")
	timeout := flag.Duration("timeout", 30*time.Minute, "предельная длительность всей проверки")
	precision := flag.Float64("minimum-precision", 0, "минимальная общая точность, обязательно")
	factPrecision := flag.Float64("minimum-fact-precision", 0, "минимальная точность каждого типа факта, обязательно")
	recall := flag.Float64("minimum-recall", 0, "минимальная полнота, обязательно")
	f1 := flag.Float64("minimum-f1", 0, "минимальная F1-мера, обязательно")
	exact := flag.Float64("minimum-exact-rate", 0, "минимальная доля точно разобранных случаев, обязательно")
	valid := flag.Float64("minimum-valid-rate", 0, "минимальная доля структурно корректных ответов, обязательно")
	evidence := flag.Float64("minimum-evidence-exact-rate", 0, "минимальная точность ссылок на доказательства, обязательно")
	p95 := flag.Int64("maximum-p95-ms", 0, "предельная задержка p95 в миллисекундах, обязательно")
	flag.Parse()
	if *precision <= 0 || *factPrecision <= 0 || *recall <= 0 || *f1 <= 0 || *exact <= 0 || *valid <= 0 || *evidence <= 0 || *p95 <= 0 {
		fatal(fmt.Errorf("все пороги качества и производительности должны быть заданы явно"))
	}
	f, err := os.Open(*datasetPath)
	fatal(err)
	defer f.Close()
	cases, digest, err := benchmark.Load(f)
	fatal(err)
	containsGolden := false
	for _, current := range cases {
		containsGolden = containsGolden || current.Split == benchmark.SplitGolden
	}
	if containsGolden || *digestPath != "" {
		if *digestPath == "" {
			fatal(fmt.Errorf("для контрольной выборки обязательна контрольная сумма"))
		}
		want, err := os.ReadFile(*digestPath)
		fatal(err)
		fields := strings.Fields(string(want))
		if len(fields) == 0 {
			fatal(fmt.Errorf("файл контрольной суммы пуст"))
		}
		fatal(benchmark.VerifyGolden(digest, fields[0]))
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, err := benchmark.Run(ctx, infrastructure.LlamaProvider{URL: *endpoint}, cases, digest, benchmark.Thresholds{
		MinimumPrecision: *precision, MinimumFactPrecision: *factPrecision, MinimumRecall: *recall, MinimumF1: *f1, MinimumExactRate: *exact, MinimumValidRate: *valid, MinimumEvidenceExactRate: *evidence, MaximumP95MS: *p95,
	})
	fatal(err)
	fatal(json.NewEncoder(os.Stdout).Encode(report))
	if !report.Passed {
		os.Exit(2)
	}
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
