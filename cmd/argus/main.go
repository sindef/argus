package main

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"argus/internal/auth"
	"argus/internal/kube"
	"argus/internal/server"
	"argus/internal/tlscert"
)

func main() {
	addr := env("ARGUS_ADDR", ":8443")
	ns := env("ARGUS_NAMESPACE", "argus-system")

	kc, err := kube.New()
	if err != nil {
		log.Fatal(err)
	}
	store := auth.NewStore(kc.Secrets(ns))
	runDir := os.Getenv("ARGUS_RUN_DIR")
	banner := consoleBanner(runDir)

	var ips []net.IP
	if ip := net.ParseIP(os.Getenv("HOST_IP")); ip != nil {
		ips = append(ips, ip)
	}
	var pki tlscert.PKI
	retry(func(ctx context.Context) (err error) {
		pki, err = tlscert.Load(ctx, kc.Secrets(ns), ips)
		return err
	})
	retry(func(ctx context.Context) error {
		pw, err := store.EnsureAdmin(ctx, readSeed(os.Getenv("ARGUS_ADMIN_PASSWORD_FILE")))
		if err == nil {
			banner(pw)
		}
		return err
	})

	certs := tlscert.NewHolder(pki.Cert)
	certs.Load(context.Background(), kc.Secrets(ns))
	srv, err := server.New(kc, store, server.Options{
		Certs:       certs,
		Banner:      banner,
		RunDir:      runDir,
		ReleaseFile: os.Getenv("ARGUS_RELEASE_FILE"),
		CAPEM:       pki.CAPEM,
		NodeName:    os.Getenv("NODE_NAME"),
	})
	if err != nil {
		log.Fatal(err)
	}
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv,
		TLSConfig:         &tls.Config{GetCertificate: certs.GetCertificate, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("listening on %s", addr)
	log.Fatal(hs.ListenAndServeTLS("", ""))
}

func consoleBanner(dir string) func(string) {
	return func(password string) {
		if dir == "" {
			if password != "" {
				log.Printf("initial admin password: %s", password)
			}
			return
		}
		text := "argus: https://\\4{br0}/\n"
		if password != "" {
			text += "initial login: admin / " + password + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "console.issue"), []byte(text+"\n"), 0o644); err != nil {
			log.Printf("banner: %v", err)
		}
	}
}

func readSeed(path string) string {
	if path == "" {
		return ""
	}
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func retry(fn func(context.Context) error) {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := fn(ctx)
		cancel()
		if err == nil {
			return
		}
		log.Print(err)
		time.Sleep(5 * time.Second)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
