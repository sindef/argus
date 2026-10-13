package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
)

const apiHeader = "X-Argus-API"

func IsAPI(r *http.Request) bool { return r.Header.Get(apiHeader) != "" }

type recorder struct {
	h    http.Header
	code int
	body bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.h }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func JSONForm(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	var body map[string]any
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			APIError(w, r, http.StatusBadRequest, errors.New("request body must be a JSON object"))
			return nil, false
		}
	}
	form := url.Values{}
	r2 := r.Clone(r.Context())
	for k, v := range body {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				form.Add(k, fmt.Sprint(e))
			}
		case bool:
			if x {
				form.Set(k, "on")
			}
		case nil:
		default:
			form.Set(k, fmt.Sprint(x))
		}
	}
	if p := form.Get("prompt"); p != "" {
		r2.Header.Set("HX-Prompt", p)
	}
	r2.Form, r2.PostForm, r2.Body = form, form, http.NoBody
	r2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r2.Header.Set("HX-Request", "true")
	r2.Header.Set(apiHeader, "1")
	return r2, true
}

func FormAPI(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r2, ok := JSONForm(w, r)
		if !ok {
			return
		}
		rec := &recorder{h: http.Header{}}
		h(rec, r2)
		code := rec.code
		if code == 0 {
			code = http.StatusOK
		}
		switch {
		case code >= 400 && strings.HasPrefix(rec.h.Get("Content-Type"), "application/json"):
			WriteJSONCode(w, code, json.RawMessage(rec.body.Bytes()))
		case code >= 400:
			msg := html.UnescapeString(strings.TrimSpace(rec.body.String()))
			if msg == "" {
				msg = http.StatusText(code)
			}
			if code == http.StatusUnprocessableEntity {
				code = http.StatusBadRequest
			}
			APIError(w, r, code, errors.New(msg))
		case strings.HasPrefix(rec.h.Get("Content-Type"), "application/json"):
			WriteJSON(w, json.RawMessage(rec.body.Bytes()))
		default:
			WriteJSON(w, map[string]string{})
		}
	}
}
