package sealed

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
)

var streamMagic = []byte("ARGUSST1")

const (
	chunk      = 1 << 20
	tagSize    = 16
	headerSize = 8 + 16 + 8
)

func streamCipher(passphrase string, salt []byte) (cipher.AEAD, error) {
	k, err := key(passphrase, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(prefix []byte, n uint32) []byte {
	b := make([]byte, 12)
	copy(b, prefix)
	binary.BigEndian.PutUint32(b[8:], n)
	return b
}

func aad(last bool) []byte {
	if last {
		return []byte{1}
	}
	return []byte{0}
}

func Encrypt(w io.Writer, r io.Reader, passphrase string) error {
	hdr := make([]byte, headerSize)
	copy(hdr, streamMagic)
	rand.Read(hdr[8:])
	gcm, err := streamCipher(passphrase, hdr[8:24])
	if err != nil {
		return err
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	br := bufio.NewReaderSize(r, chunk)
	buf := make([]byte, chunk)
	for n := uint32(0); ; n++ {
		k, err := io.ReadFull(br, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		}
		_, perr := br.Peek(1)
		last := k < chunk || perr == io.EOF
		if _, err := w.Write(gcm.Seal(nil, nonce(hdr[24:], n), buf[:k], aad(last))); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

type reader struct {
	r      *bufio.Reader
	gcm    cipher.AEAD
	prefix []byte
	n      uint32
	buf    []byte
	done   bool
}

func Decrypt(r io.Reader, passphrase string) (io.Reader, bool, error) {
	br := bufio.NewReaderSize(r, chunk+tagSize)
	hdr, err := br.Peek(headerSize)
	switch {
	case errors.Is(err, io.EOF):
		return br, false, nil
	case err != nil:
		return nil, false, err
	case !bytes.Equal(hdr[:8], streamMagic):
		return br, false, nil
	}
	gcm, err := streamCipher(passphrase, hdr[8:24])
	if err != nil {
		return nil, true, err
	}
	prefix := bytes.Clone(hdr[24:])
	if _, err := br.Discard(headerSize); err != nil {
		return nil, true, err
	}
	return &reader{r: br, gcm: gcm, prefix: prefix}, true, nil
}

func (d *reader) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		if d.done {
			return 0, io.EOF
		}
		ct := make([]byte, chunk+tagSize)
		k, err := io.ReadFull(d.r, ct)
		if err != nil && err != io.ErrUnexpectedEOF {
			if err == io.EOF {
				return 0, errors.New("encrypted backup is truncated")
			}
			return 0, err
		}
		_, perr := d.r.Peek(1)
		last := perr == io.EOF
		plain, oerr := d.gcm.Open(nil, nonce(d.prefix, d.n), ct[:k], aad(last))
		if oerr != nil {
			return 0, errors.New("wrong backup passphrase or damaged backup")
		}
		d.n++
		d.buf, d.done = plain, last
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

func PlainSize(size int64) int64 {
	body := size - headerSize
	if body < tagSize {
		return size
	}
	chunks := (body + chunk + tagSize - 1) / (chunk + tagSize)
	return body - chunks*tagSize
}
