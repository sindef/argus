package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"argus/internal/auth"
	"argus/internal/filestore"
	"argus/internal/manager"
	"argus/internal/sealed"
	"argus/internal/server"
	"argus/internal/tlscert"
)

func main() {
	addr := env("ARGUS_ADDR", ":8443")
	data := env("ARGUS_DATA", "/var/lib/argus-manager")
	restoreFile := flag.String("restore", "", "restore an encrypted configuration backup into ARGUS_DATA and exit; passphrase from ARGUS_BACKUP_PASSPHRASE")
	force := flag.Bool("force", false, "with -restore, overwrite an existing configuration")
	flag.Parse()
	if *restoreFile != "" {
		if err := restore(*restoreFile, data, os.Getenv("ARGUS_BACKUP_PASSPHRASE"), *force); err != nil {
			log.Fatal(err)
		}
		log.Printf("configuration restored into %s", data)
		return
	}
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
		f := filepath.Join(data, "initial-admin-password")
		if err := os.WriteFile(f, []byte(pw+"\n"), 0o600); err != nil {
			log.Fatal(err)
		}
		log.Printf("initial login: admin, password in %s (delete it after signing in)", f)
	}
	pki, err := tlscert.Load(ctx, fs, nil)
	if err != nil {
		log.Fatal(err)
	}
	mgr.Start()
	certs := tlscert.NewHolder(pki.Cert)
	certs.Load(ctx, fs)
	srv, err := server.New(nil, store, server.Options{Banner: func(string) {}, Manager: mgr, Certs: certs, DataDir: data})
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
	go func() {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer stop()
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	if err := hs.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func restore(file, data, passphrase string, force bool) error {
	if passphrase == "" {
		return errors.New("set ARGUS_BACKUP_PASSPHRASE")
	}
	if _, err := os.Stat(filepath.Join(data, "argus-nodes.json")); err == nil && !force {
		return fmt.Errorf("%s already holds a configuration; stop Argus Manager and pass -force to overwrite it", data)
	}
	enc, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	plain, err := sealed.Open(passphrase, enc)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(data, 0o700); err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Base(h.Name)
		if name != h.Name || !strings.HasSuffix(name, ".json") {
			return fmt.Errorf("unexpected entry %q in backup", h.Name)
		}
		b, err := io.ReadAll(io.LimitReader(tr, 64<<20))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(data, name), b, 0o600); err != nil {
			return err
		}
	}
}
