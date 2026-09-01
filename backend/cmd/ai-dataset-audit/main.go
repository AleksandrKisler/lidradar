// Команда ai-dataset-audit проверяет набор этапа 15 без обращения к модели.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"lidradar/backend/internal/ai/benchmark"
)

func main() {
	trainPath := flag.String("train", "models/datasets/train_v1.jsonl", "обучающая выборка")
	validationPath := flag.String("validation", "models/datasets/validation_v1.jsonl", "проверочная выборка")
	goldenPath := flag.String("golden", "models/datasets/golden_v1.jsonl", "контрольная выборка")
	checksumPath := flag.String("golden-checksum", "models/datasets/golden_v1.sha256", "контрольная сумма контрольной выборки")
	flag.Parse()

	var all []benchmark.Case
	for _, item := range []struct {
		path  string
		split benchmark.Split
	}{
		{*trainPath, benchmark.SplitTrain},
		{*validationPath, benchmark.SplitValidation},
		{*goldenPath, benchmark.SplitGolden},
	} {
		file, err := os.Open(item.path)
		fatal(err)
		cases, digest, err := benchmark.Load(file)
		_ = file.Close()
		fatal(err)
		for _, current := range cases {
			if current.Split != item.split {
				fatal(fmt.Errorf("файл %s содержит случай из выборки %s", item.path, current.Split))
			}
		}
		if item.split == benchmark.SplitGolden {
			checksum, err := os.ReadFile(*checksumPath)
			fatal(err)
			fields := strings.Fields(string(checksum))
			if len(fields) == 0 {
				fatal(fmt.Errorf("файл контрольной суммы пуст"))
			}
			fatal(benchmark.VerifyGolden(digest, fields[0]))
		}
		all = append(all, cases...)
	}

	report, err := benchmark.AuditCases(all)
	fatal(err)
	if report.Cases < 300 || report.Cases > 500 {
		fatal(fmt.Errorf("ТЗ требует от 300 до 500 случаев, получено %d", report.Cases))
	}
	if report.SplitCounts[benchmark.SplitTrain] == 0 || report.SplitCounts[benchmark.SplitValidation] == 0 || report.SplitCounts[benchmark.SplitGolden] == 0 {
		fatal(fmt.Errorf("каждая выборка должна быть непустой"))
	}
	fatal(json.NewEncoder(os.Stdout).Encode(report))
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
