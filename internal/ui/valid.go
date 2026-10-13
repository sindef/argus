package ui

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var ValidObject = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func IntField(r *http.Request, field string, min, max int, dst *int) error {
	n, err := strconv.Atoi(r.FormValue(field))
	if err != nil || n < min || n > max {
		return fmt.Errorf("%s must be %d-%d", field, min, max)
	}
	*dst = n
	return nil
}

func NewToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func Fields(v string) []string { return strings.Fields(strings.ReplaceAll(v, ",", " ")) }

func Gibibytes(v string) (string, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 4096 {
		return "", errors.New("size must be 1-4096 GiB")
	}
	return fmt.Sprintf("%dGi", n), nil
}

var ValidImage = regexp.MustCompile(`^[A-Za-z0-9./:@_-]+$`)
