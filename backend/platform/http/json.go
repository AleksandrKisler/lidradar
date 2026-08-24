package httpplatform

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const maxJSONBody = 64 << 10

// DecodeJSON accepts exactly one bounded JSON value and rejects unknown fields.
func DecodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}
