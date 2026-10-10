package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// ServerInfo привязывает отчёт к серверу, который отвечал на запросы: сборка
// llama.cpp, модель и параметры генерации, которые сервер увидел в последнем
// запросе. Сведения читаются запросами GET, ошибка чтения не отменяет проверку:
// она записывается в Errors, и привязка считается неполной.
type ServerInfo struct {
	CapturedAt       string    `json:"capturedAt"`
	BuildInfo        string    `json:"buildInfo,omitempty"`
	ModelPath        string    `json:"modelPath,omitempty"`
	ModelFtype       string    `json:"modelFtype,omitempty"`
	ModelSizeBytes   int64     `json:"modelSizeBytes,omitempty"`
	ModelParameters  int64     `json:"modelParameters,omitempty"`
	ContextSize      int       `json:"contextSize,omitempty"`
	TotalSlots       int       `json:"totalSlots,omitempty"`
	ObservedSampling *Sampling `json:"observedSampling,omitempty"`
	Errors           []string  `json:"errors,omitempty"`
}

// Sampling — параметры, с которыми сервер выполнил последний запрос проверки.
type Sampling struct {
	Seed             int64   `json:"seed"`
	Temperature      float64 `json:"temperature"`
	TopP             float64 `json:"topP"`
	TopK             int     `json:"topK"`
	MinP             float64 `json:"minP"`
	PresencePenalty  float64 `json:"presencePenalty"`
	ThinkingDisabled bool    `json:"thinkingDisabled"`
}

// ServerBase получает адрес сервера из маршрута chat completions.
func ServerBase(endpoint string) string {
	return strings.TrimSuffix(strings.TrimRight(endpoint, "/"), "/v1/chat/completions")
}

// CaptureServer читает сборку, модель и размер контекста. Параметры генерации
// добавляет ObserveSampling после прогона: сервер хранит их от последнего запроса.
func CaptureServer(ctx context.Context, client *http.Client, endpoint string) *ServerInfo {
	info := &ServerInfo{CapturedAt: time.Now().UTC().Format(time.RFC3339)}
	base := ServerBase(endpoint)
	var props struct {
		BuildInfo  string `json:"build_info"`
		ModelPath  string `json:"model_path"`
		ModelFtype string `json:"model_ftype"`
		TotalSlots int    `json:"total_slots"`
		Defaults   struct {
			ContextSize int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := getJSON(ctx, client, base+"/props", &props); err != nil {
		info.Errors = append(info.Errors, err.Error())
	} else {
		info.BuildInfo, info.ModelPath, info.ModelFtype = props.BuildInfo, props.ModelPath, props.ModelFtype
		info.TotalSlots, info.ContextSize = props.TotalSlots, props.Defaults.ContextSize
	}
	var models struct {
		Data []struct {
			Meta struct {
				Parameters  int64 `json:"n_params"`
				Size        int64 `json:"size"`
				ContextSize int   `json:"n_ctx"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := getJSON(ctx, client, base+"/v1/models", &models); err != nil {
		info.Errors = append(info.Errors, err.Error())
	} else if len(models.Data) > 0 {
		info.ModelParameters, info.ModelSizeBytes = models.Data[0].Meta.Parameters, models.Data[0].Meta.Size
		if info.ContextSize == 0 {
			info.ContextSize = models.Data[0].Meta.ContextSize
		}
	}
	return info
}

// ObserveSampling читает параметры последнего запроса: независимое с сервера
// подтверждение того, что прогон шёл с зафиксированными в манифесте значениями.
func (info *ServerInfo) ObserveSampling(ctx context.Context, client *http.Client, endpoint string) {
	var slots []struct {
		Params struct {
			Seed             int64   `json:"seed"`
			Temperature      float64 `json:"temperature"`
			TopP             float64 `json:"top_p"`
			TopK             int     `json:"top_k"`
			MinP             float64 `json:"min_p"`
			PresencePenalty  float64 `json:"presence_penalty"`
			GenerationPrompt string  `json:"generation_prompt"`
		} `json:"params"`
	}
	if err := getJSON(ctx, client, ServerBase(endpoint)+"/slots", &slots); err != nil {
		info.Errors = append(info.Errors, err.Error())
		return
	}
	if len(slots) == 0 {
		info.Errors = append(info.Errors, "/slots: список пуст")
		return
	}
	p := slots[0].Params
	info.ObservedSampling = &Sampling{
		Seed: p.Seed, Temperature: round6(p.Temperature), TopP: round6(p.TopP), TopK: p.TopK,
		MinP: round6(p.MinP), PresencePenalty: round6(p.PresencePenalty),
		ThinkingDisabled: strings.Contains(p.GenerationPrompt, "<think>\n\n</think>"),
	}
}

// round6 убирает хвост float32 в ответе сервера (0.20000000298 → 0.2).
func round6(value float64) float64 { return math.Round(value*1e6) / 1e6 }

func getJSON(ctx context.Context, client *http.Client, address string, target any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%s: %s", pathOf(address), Describe(err))
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("%s: состояние %d", pathOf(address), response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target); err != nil {
		return fmt.Errorf("%s: %w", pathOf(address), err)
	}
	return nil
}

// pathOf оставляет в тексте ошибки только путь: адрес узла в отчёт не попадает.
func pathOf(address string) string {
	if i := strings.Index(address, "://"); i >= 0 {
		address = address[i+3:]
	}
	if i := strings.Index(address, "/"); i >= 0 {
		return address[i:]
	}
	return address
}
