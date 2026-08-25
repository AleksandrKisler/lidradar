package httpplatform

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

type jsonFixture struct {
	Name string `json:"name"`
}

func TestDecodeJSONRejectsAmbiguousAndUnsafeBodies(t *testing.T) {
	for name, payload := range map[string][]byte{
		"неизвестное поле":   []byte(`{"name":"ok","admin":true}`),
		"несколько значений": []byte(`{"name":"one"}{"name":"two"}`),
		"неверный UTF-8":     append([]byte(`{"name":"`), 0xff, '"', '}'),
		"нулевой байт":       []byte(`{"name":"bad\u0000value"}`),
		"слишком большой":    []byte(`{"name":"` + strings.Repeat("x", maxJSONBody) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/", bytes.NewReader(payload))
			if err := DecodeJSON(httptest.NewRecorder(), request, &jsonFixture{}); err == nil {
				t.Fatal("опасный JSON принят")
			}
		})
	}
}

func FuzzDecodeJSONNeverPanics(f *testing.F) {
	for _, payload := range [][]byte{
		[]byte(`{"name":"обычный запрос"}`),
		[]byte(`{"name":"' OR 1=1; --"}`),
		[]byte(`{"name":"\u0000"}`),
		{0xff, 0xfe, 0xfd},
		[]byte(strings.Repeat("[", 10_000) + strings.Repeat("]", 10_000)),
	} {
		f.Add(payload)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > maxJSONBody*2 {
			t.Skip()
		}
		request := httptest.NewRequest("POST", "/", bytes.NewReader(payload))
		_ = DecodeJSON(httptest.NewRecorder(), request, &jsonFixture{})
	})
}
