// Команда ai-scan-probe запускает только проход по обещаниям (ADR 0055) по набору и пишет вероятность promise по каждому
// проверяемому сообщению (для сообщений вне словаря — с подтверждением вторым вариантом, как в рабочем пути) и состояние словаря
// каждого исходящего сообщения. Тексты в вывод не попадают. Это инструмент настройки:
// качество ревизии измеряет ai-benchmark, а здесь видно, на чём именно проход ошибается, и порог можно подобрать без второго вызова.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"lidradar/backend/internal/ai/application"
	"lidradar/backend/internal/ai/benchmark"
	"lidradar/backend/internal/ai/infrastructure"
)

type line struct {
	ID            string             `json:"id"`
	Probabilities map[string]float64 `json:"probabilities"`
	Support       map[string]string  `json:"support"`
	Stated        map[string]bool    `json:"stated"`
	Milliseconds  int64              `json:"milliseconds"`
}

var supportNames = map[application.LexicalSupport]string{
	application.LexicalAdmitted: "ADMITTED",
	application.LexicalOpen:     "OPEN",
	application.LexicalRuledOut: "RULED_OUT",
}

func main() {
	dataset := flag.String("dataset", "", "набор в формате lidradar-ai-benchmark.v1")
	endpoint := flag.String("endpoint", "http://127.0.0.1:8080/v1/chat/completions", "маршрут llama.cpp")
	timeout := flag.Duration("timeout", 30*time.Minute, "предельная длительность всего прогона")
	flag.Parse()
	file, err := os.Open(*dataset)
	fatal(err)
	defer file.Close()
	cases, _, err := benchmark.Load(file)
	fatal(err)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	provider := infrastructure.LlamaProvider{URL: *endpoint, Client: http.DefaultClient}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	for _, c := range cases {
		cs := c
		cs.Input.PromptVersion = application.AnalysisPromptV9
		prompt, err := application.EncodeAnalysisRequest(cs.Input)
		fatal(err)
		started := time.Now()
		probabilities, err := provider.PromiseProbabilities(ctx, prompt)
		fatal(err)
		support, stated := map[string]string{}, map[string]bool{}
		for _, m := range c.Input.Messages {
			if m.Direction == "OUTGOING" {
				support[m.ID] = supportNames[application.CommitmentSupport(m.Body)]
				stated[m.ID] = application.CommitmentStated(m.Body)
			}
		}
		fatal(encoder.Encode(line{ID: c.ID, Probabilities: probabilities, Support: support, Stated: stated, Milliseconds: time.Since(started).Milliseconds()}))
	}
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
