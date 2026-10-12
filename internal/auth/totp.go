package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const totpStep = 30

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func NewTOTPSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return b32.EncodeToString(b)
}

func TOTPURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	return "otpauth://totp/" + label + "?" + url.Values{"secret": {secret}, "issuer": {issuer}}.Encode()
}

func totpCode(key []byte, counter int64) string {
	m := hmac.New(sha1.New, key)
	binary.Write(m, binary.BigEndian, counter)
	sum := m.Sum(nil)
	o := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[o:o+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1000000)
}

func CheckTOTP(secret, code string, now time.Time) (int64, bool) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if err != nil || len(code) != 6 {
		return 0, false
	}
	c := now.Unix() / totpStep
	for _, d := range []int64{0, -1, 1} {
		if subtle.ConstantTimeCompare([]byte(totpCode(key, c+d)), []byte(code)) == 1 {
			return c + d, true
		}
	}
	return 0, false
}

func (s *Store) SetTOTP(ctx context.Context, name, secret string) error {
	return s.update(ctx, name, func(u *User) error {
		if u.SSO {
			return ErrSSO
		}
		u.TOTP = secret
		return nil
	})
}

func (s *Store) StartSession(ctx context.Context, u User, remember bool) string {
	role, scopes := s.Access(ctx, u)
	return s.NewSession(Session{User: u.Name, Role: role, MustChange: u.MustChange, Scopes: scopes}, remember)
}
