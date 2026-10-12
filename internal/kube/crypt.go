package kube

import (
	"crypto/rand"
	"crypto/sha512"
	"strings"
)

const cryptAlphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func cryptSalt() string {
	b := make([]byte, 16)
	rand.Read(b)
	for i := range b {
		b[i] = cryptAlphabet[int(b[i])%len(cryptAlphabet)]
	}
	return string(b)
}

func repeatTo(src []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, src[:min(len(src), n-len(out))]...)
	}
	return out
}

func sha512Crypt(password, salt string) string {
	const rounds = 5000
	p, s := []byte(password), []byte(salt)
	if len(s) > 16 {
		s = s[:16]
	}
	b := sha512.New()
	b.Write(p)
	b.Write(s)
	b.Write(p)
	sumB := b.Sum(nil)

	a := sha512.New()
	a.Write(p)
	a.Write(s)
	a.Write(repeatTo(sumB, len(p)))
	for i := len(p); i > 0; i >>= 1 {
		if i&1 != 0 {
			a.Write(sumB)
		} else {
			a.Write(p)
		}
	}
	sumA := a.Sum(nil)

	dp := sha512.New()
	for range len(p) {
		dp.Write(p)
	}
	pBytes := repeatTo(dp.Sum(nil), len(p))

	ds := sha512.New()
	for range 16 + int(sumA[0]) {
		ds.Write(s)
	}
	sBytes := repeatTo(ds.Sum(nil), len(s))

	c := sumA
	for i := range rounds {
		h := sha512.New()
		if i&1 != 0 {
			h.Write(pBytes)
		} else {
			h.Write(c)
		}
		if i%3 != 0 {
			h.Write(sBytes)
		}
		if i%7 != 0 {
			h.Write(pBytes)
		}
		if i&1 != 0 {
			h.Write(c)
		} else {
			h.Write(pBytes)
		}
		c = h.Sum(nil)
	}

	var out strings.Builder
	enc := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for range n {
			out.WriteByte(cryptAlphabet[w&0x3f])
			w >>= 6
		}
	}
	order := [][3]int{{0, 21, 42}, {22, 43, 1}, {44, 2, 23}, {3, 24, 45}, {25, 46, 4}, {47, 5, 26}, {6, 27, 48}, {28, 49, 7}, {50, 8, 29}, {9, 30, 51}, {31, 52, 10}, {53, 11, 32}, {12, 33, 54}, {34, 55, 13}, {56, 14, 35}, {15, 36, 57}, {37, 58, 16}, {59, 17, 38}, {18, 39, 60}, {40, 61, 19}, {62, 20, 41}}
	for _, o := range order {
		enc(c[o[0]], c[o[1]], c[o[2]], 4)
	}
	enc(0, 0, c[63], 2)
	return "$6$" + string(s) + "$" + out.String()
}
