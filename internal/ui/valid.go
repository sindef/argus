package ui

import (
	"crypto/rand"
	"encoding/hex"
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
