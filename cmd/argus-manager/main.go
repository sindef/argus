package main

import (
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"argus/internal/auth"
	"argus/internal/filestore"
	"argus/internal/manager"
	"argus/internal/server"
	"argus/internal/tlscert"
)

func main() {
	addr := env("ARGUS_ADDR", ":8443")
	data := env("ARGUS_DATA", "/var/lib/argus-manager")
	fs, err := filestore.New(data)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mgr := manager.New(fs, filepath.Join(data, "metrics.gob"))
	if err := mgr.EnsureDefaultOrg(ctx); err != nil {
		log.Fatal(err)
	}
	store := auth.NewStore(fs)
	pw, err := store.ForOrg(manager.DefaultOrg).EnsureAdmin(ctx, "")
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
	mgr.Start()
	certs := tlscert.NewHolder(pki.Cert)
	certs.Load(ctx, fs)
	srv, err := server.New(nil, store, server.Options{Banner: func(string) {}, Manager: mgr, Certs: certs})
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

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
