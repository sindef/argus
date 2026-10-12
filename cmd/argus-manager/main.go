package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
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
	"argus/internal/dbstore"
	"argus/internal/manager"
	"argus/internal/sealed"
	"argus/internal/server"
	"argus/internal/tlscert"
)

func main() {
	syscall.Umask(0o077)
	addr := env("ARGUS_ADDR", ":8443")
	data := env("ARGUS_DATA", "/var/lib/argus-manager")
	keyFile := env("ARGUS_KEY_FILE", filepath.Join(data, "argus.key"))
	restoreFile := flag.String("restore", "", "restore an encrypted configuration backup into ARGUS_DATA and exit; passphrase from ARGUS_BACKUP_PASSPHRASE")
	force := flag.Bool("force", false, "with -restore, overwrite an existing configuration")
	flag.Parse()
	if *restoreFile != "" {
		if err := restore(*restoreFile, data, keyFile, os.Getenv("ARGUS_BACKUP_PASSPHRASE"), *force); err != nil {
			log.Fatal(err)
		}
		log.Printf("configuration restored into %s", data)
		return
	}
	fs, err := openStore(data, keyFile)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if n, err := fs.ImportFiles(ctx, data); err != nil {
		log.Fatal(err)
	} else if n > 0 {
		log.Printf("moved %d configuration files into the encrypted database", n)
	}
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

func openStore(data, keyFile string) (*dbstore.Store, error) {
	if err := os.MkdirAll(data, 0o700); err != nil {
		return nil, err
	}
	return dbstore.Open(filepath.Join(data, "argus.db"), keyFile)
}

func restore(file, data, keyFile, passphrase string, force bool) error {
	if passphrase == "" {
		return errors.New("set ARGUS_BACKUP_PASSPHRASE")
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
	recs := map[string]dbstore.Record{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Base(h.Name)
		if name != h.Name || !strings.HasSuffix(name, ".json") {
			return fmt.Errorf("unexpected entry %q in backup", h.Name)
		}
		var rec dbstore.Record
		if err := json.NewDecoder(io.LimitReader(tr, 64<<20)).Decode(&rec); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		recs[strings.TrimSuffix(name, ".json")] = rec
	}
	st, err := openStore(data, keyFile)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	if empty, err := st.Empty(ctx); err != nil {
		return err
	} else if !empty && !force {
		return fmt.Errorf("%s already holds a configuration; stop Argus Manager and pass -force to overwrite it", data)
	}
	return st.Replace(ctx, recs)
}
