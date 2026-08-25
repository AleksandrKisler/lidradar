package httpplatform

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"unicode/utf8"
)

const maxJSONBody = 64 << 10

// DecodeJSON accepts exactly one bounded JSON value and rejects unknown fields.
func DecodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		return err
	}
	if !utf8.Valid(payload) {
		return errors.New("request body must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
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
	if containsNUL(reflect.ValueOf(destination)) {
		return errors.New("request body contains a zero byte")
	}
	return nil
}

func containsNUL(value reflect.Value) bool {
	if !value.IsValid() {
		return false
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		return !value.IsNil() && containsNUL(value.Elem())
	case reflect.String:
		return strings.ContainsRune(value.String(), '\x00')
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			if containsNUL(value.Field(index)) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		// []byte может содержать самостоятельный двоичный формат; строковые поля
		// и составные коллекции проверяются поэлементно.
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return false
		}
		for index := 0; index < value.Len(); index++ {
			if containsNUL(value.Index(index)) {
				return true
			}
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if containsNUL(iterator.Key()) || containsNUL(iterator.Value()) {
				return true
			}
		}
	}
	return false
}
