package auth

import (
	"encoding/base32"
	"testing"
	"time"
)

func TestTOTPRFC6238(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	for ts, code := range map[int64]string{59: "287082", 1111111109: "081804", 1234567890: "005924", 2000000000: "279037"} {
		if _, ok := CheckTOTP(secret, code, time.Unix(ts, 0)); !ok {
			t.Errorf("t=%d: %s rejected", ts, code)
		}
	}
	if _, ok := CheckTOTP(secret, "287082", time.Unix(59+120, 0)); ok {
		t.Error("stale code accepted")
	}
}
