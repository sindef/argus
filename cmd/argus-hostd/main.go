package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"argus/internal/hostd"
)

func listener(path string) (net.Listener, error) {
	if pid, _ := strconv.Atoi(os.Getenv("LISTEN_PID")); pid == os.Getpid() && os.Getenv("LISTEN_FDS") == "1" {
		return net.FileListener(os.NewFile(3, "hostd.sock"))
	}
	os.Remove(path)
	return net.Listen("unix", path)
}

func main() {
	path := flag.String("socket", "/run/argus/hostd.sock", "socket path when not socket-activated")
	idle := flag.Duration("idle", time.Minute, "exit after this long without requests")
	rollback := flag.Bool("rollback", false, "roll back an unconfirmed network change and exit")
	flag.Parse()
	if *rollback {
		if err := hostd.Rollback(context.Background()); err != nil {
			log.Fatal(err)
		}
		log.Print("network change rolled back")
		return
	}
	l, err := listener(*path)
	if err != nil {
		log.Fatal(err)
	}
	srv := hostd.NewServer()
	go func() {
		for range time.Tick(10 * time.Second) {
			if srv.Idle(*idle) {
				os.Exit(0)
			}
		}
	}()
	log.Fatal((&http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}).Serve(l))
}
