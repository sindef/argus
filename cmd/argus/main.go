package main

import (
	"context"
	"crypto/tls"
	"log"
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

	var cert tls.Certificate
	retry(func(ctx context.Context) (err error) {
		cert, err = tlscert.Load(ctx, kc.Secrets(ns))
		return err
	})
	retry(func(ctx context.Context) error {
		pw, err := store.EnsureAdmin(ctx, readSeed(os.Getenv("ARGUS_ADMIN_PASSWORD_FILE")))
		if err == nil {
			banner(pw)
		}
		return err
	})

	srv, err := server.New(kc, store, server.Options{Banner: banner, RunDir: runDir, ReleaseFile: os.Getenv("ARGUS_RELEASE_FILE")})
	if err != nil {
		log.Fatal(err)
	}
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
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
