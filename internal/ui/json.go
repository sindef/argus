package ui

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func WriteJSON(w http.ResponseWriter, v any) {
	WriteJSONCode(w, http.StatusOK, v)
}

func WriteJSONCode(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func APIError(w http.ResponseWriter, r *http.Request, status int, err error) {
	log.Printf("%s: %s %s: %v", Session(r).User, r.Method, r.URL.Path, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Core) APIOp(w http.ResponseWriter, r *http.Request, in any, fn func() (any, error)) {
	if in != nil {
		if err := DecodeJSON(r.Body, in); err != nil {
			APIError(w, r, http.StatusBadRequest, err)
			return
		}
	}
	out, err := fn()
	if errors.Is(err, ErrUnknownAction) {
		APIError(w, r, http.StatusBadRequest, err)
		return
	}
	if apierrors.IsNotFound(err) {
		APIError(w, r, http.StatusNotFound, err)
		return
	}
	if err != nil {
		APIError(w, r, http.StatusConflict, err)
		return
	}
	if out == nil {
		out = map[string]string{}
	}
	WriteJSON(w, out)
}

func DecodeJSON(r io.Reader, v any) error {
	if err := json.NewDecoder(r).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

var ErrUnknownAction = errors.New("unknown action")
