package tlscert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const secretName = "argus-pki"

type Secrets interface {
	Get(ctx context.Context, name string) (map[string][]byte, string, error)
	Create(ctx context.Context, name, typ string, data map[string][]byte) error
	Update(ctx context.Context, name, resourceVersion string, data map[string][]byte) error
}

type PKI struct {
	Cert  tls.Certificate
	CAPEM []byte
}

func Load(ctx context.Context, secrets Secrets, ips []net.IP, names ...string) (PKI, error) {
	data, rv, err := secrets.Get(ctx, secretName)
	if err != nil && !apierrors.IsNotFound(err) {
		return PKI{}, err
	}
	if apierrors.IsNotFound(err) {
		caCert, caKey, err := newCA()
		if err != nil {
			return PKI{}, err
		}
		data = map[string][]byte{"ca.crt": caCert, "ca.key": caKey}
	}
	if !covers(data["tls.crt"], ips, names) {
		crt, key, err := newLeaf(data["ca.crt"], data["ca.key"], ips, names)
		if err != nil {
			return PKI{}, err
		}
		data["tls.crt"], data["tls.key"] = crt, key
		if rv == "" {
			err = secrets.Create(ctx, secretName, "Opaque", data)
		} else {
			err = secrets.Update(ctx, secretName, rv, data)
		}
		if err != nil {
			return PKI{}, err
		}
	}
	cert, err := tls.X509KeyPair(append(data["tls.crt"], data["ca.crt"]...), data["tls.key"])
	return PKI{Cert: cert, CAPEM: data["ca.crt"]}, err
}

func Fingerprint(certPEM []byte) string {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return ""
	}
	sum := sha256.Sum256(b.Bytes)
	return hex.EncodeToString(sum[:])
}

func covers(certPEM []byte, ips []net.IP, names []string) bool {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return false
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil || time.Now().After(c.NotAfter) {
		return false
	}
	for _, ip := range ips {
		found := false
		for _, have := range c.IPAddresses {
			if have.Equal(ip) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	for _, n := range names {
		if !slices.Contains(c.DNSNames, n) {
			return false
		}
	}
	return true
}

func newCA() ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "argus CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	return encode(tmpl, tmpl, key, key)
}

func newLeaf(caPEM, caKeyPEM []byte, ips []net.IP, names []string) ([]byte, []byte, error) {
	cb, _ := pem.Decode(caPEM)
	kb, _ := pem.Decode(caKeyPEM)
	if cb == nil || kb == nil {
		return nil, nil, errors.New("invalid CA")
	}
	ca, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	caKey, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "argus"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     append([]string{"argus"}, names...),
		IPAddresses:  append([]net.IP{net.ParseIP("127.0.0.1")}, ips...),
	}
	return encode(tmpl, ca, key, caKey)
}

func encode(tmpl, parent *x509.Certificate, key, signer *ecdsa.PrivateKey) ([]byte, []byte, error) {
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	return n
}

type Holder struct {
	builtin atomic.Pointer[tls.Certificate]
	custom  atomic.Pointer[tls.Certificate]
}

func NewHolder(builtin tls.Certificate) *Holder {
	h := &Holder{}
	h.builtin.Store(&builtin)
	return h
}

func (h *Holder) SetBuiltin(c tls.Certificate) { h.builtin.Store(&c) }

func (h *Holder) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := h.custom.Load(); c != nil && hello.ServerName != "" && hello.ServerName != InternalName {
		return c, nil
	}
	return h.builtin.Load(), nil
}

const InternalName = "argus"

func (h *Holder) Custom() *x509.Certificate {
	c := h.custom.Load()
	if c == nil {
		return nil
	}
	leaf, _ := x509.ParseCertificate(c.Certificate[0])
	return leaf
}

const customSecret = "argus-tls-custom"

func (h *Holder) Load(ctx context.Context, secrets Secrets) {
	data, _, err := secrets.Get(ctx, customSecret)
	if err != nil || len(data["cert"]) == 0 {
		return
	}
	if c, err := tls.X509KeyPair(data["cert"], data["key"]); err == nil {
		h.custom.Store(&c)
	}
}

func (h *Holder) Set(ctx context.Context, secrets Secrets, certPEM, keyPEM []byte) error {
	var c *tls.Certificate
	if len(certPEM) > 0 {
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return fmt.Errorf("certificate and key do not match or are not PEM: %w", err)
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return err
		}
		if time.Now().After(leaf.NotAfter) {
			return errors.New("certificate has expired")
		}
		c = &pair
	}
	data := map[string][]byte{"cert": certPEM, "key": keyPEM}
	_, rv, err := secrets.Get(ctx, customSecret)
	switch {
	case apierrors.IsNotFound(err):
		err = secrets.Create(ctx, customSecret, "Opaque", data)
	case err == nil:
		err = secrets.Update(ctx, customSecret, rv, data)
	}
	if err != nil {
		return err
	}
	h.custom.Store(c)
	return nil
}
