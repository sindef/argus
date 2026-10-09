package main

import (
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"os"
	"time"

	"argus/internal/auth"
	"argus/internal/center"
	"argus/internal/filestore"
	"argus/internal/server"
	"argus/internal/tlscert"
)

func main() {
	addr := env("ARGUS_ADDR", ":8443")
	fs, err := filestore.New(env("ARGUS_DATA", "/var/lib/argus-center"))
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := auth.NewStore(fs)
	pw, err := store.EnsureAdmin(ctx, "")
	if err != nil {
		log.Fatal(err)
	}
	if pw != "" {
		log.Printf("initial login: admin / %s", pw)
	}
	pki, err := tlscert.Load(ctx, fs, nil)
	if err != nil {
		log.Fatal(err)
	}
	srv, err := server.New(nil, store, server.Options{Banner: func(string) {}, Center: center.New(fs)})
	if err != nil {
		log.Fatal(err)
	}
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{pki.Cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("listening on %s", addr)
	log.Fatal(hs.ListenAndServeTLS("", ""))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
