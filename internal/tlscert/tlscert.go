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
	"math/big"
	"net"
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

func Load(ctx context.Context, secrets Secrets, ips []net.IP) (PKI, error) {
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
	if !covers(data["tls.crt"], ips) {
		crt, key, err := newLeaf(data["ca.crt"], data["ca.key"], ips)
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

func covers(certPEM []byte, ips []net.IP) bool {
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

func newLeaf(caPEM, caKeyPEM []byte, ips []net.IP) ([]byte, []byte, error) {
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
		DNSNames:     []string{"argus"},
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
