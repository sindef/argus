package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"argus/internal/auth"
	"argus/internal/dbstore"
	"argus/internal/lifecycle"
	"argus/internal/manager"
	"argus/internal/managerserver"
	"argus/internal/tlscert"
	"argus/internal/ui"
)

func main() {
	ui.TrustProxies(os.Getenv("ARGUS_TRUSTED_PROXIES"))
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
	if fs.Shared() {
		log.Printf("standby: waiting to become the active Manager")
		host, _ := os.Hostname()
		lost, err := fs.Lead(context.Background(), fmt.Sprintf("%s/%d/%d", host, os.Getpid(), time.Now().UnixNano()))
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("active: this Manager now serves requests")
		go func() {
			log.Fatalf("lost the active role, exiting so a standby can take over: %v", <-lost)
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if n, err := fs.ImportFiles(ctx, data); err != nil {
		log.Fatal(err)
	} else if n > 0 {
		log.Printf("moved %d configuration files into the encrypted database", n)
	}
	mgr := manager.New(fs, filepath.Join(data, "metrics.gob"), envInt("ARGUS_REPLICATIONS", 2))
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
	mgr.Prepare(ctx)
	certs := tlscert.NewHolder(pki.Cert)
	certs.Load(ctx, fs)
	cancel()
	srv, err := managerserver.New(store, managerserver.Options{Banner: func(string) {}, Manager: mgr, Certs: certs, DataDir: data, Addr: addr})
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
	if err := lifecycle.Serve(hs, mgr.Run, srv.Run); err != nil {
		log.Fatal(err)
	}
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return n
	}
	return def
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
	if url := os.Getenv("ARGUS_DATABASE_URL"); url != "" {
		return dbstore.Open(url, keyFile)
	}
	return dbstore.Open(filepath.Join(data, "argus.db"), keyFile)
}

func restore(file, data, keyFile, passphrase string, force bool) error {
	if passphrase == "" {
		return errors.New("set ARGUS_BACKUP_PASSPHRASE")
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
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
	recs, manifest, err := dbstore.Unpack(f, passphrase, data)
	if err != nil {
		return err
	}
	var man struct {
		Included          bool
		Images, Templates []string
	}
	if json.Unmarshal(manifest, &man) == nil && !man.Included && len(man.Images)+len(man.Templates) > 0 {
		log.Printf("library content is not in this backup; add these again: %s", strings.Join(slices.Concat(man.Images, man.Templates), ", "))
	}
	return st.Replace(ctx, recs)
}
