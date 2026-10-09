package main

import (
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"os"
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
	var cert tls.Certificate
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cert, err = tlscert.Load(ctx, kc.Secrets(ns))
		cancel()
		if err == nil {
			break
		}
		log.Print(err)
		time.Sleep(5 * time.Second)
	}
	srv, err := server.New(kc, auth.NewStore(kc.Secrets(ns)))
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

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
