package lifecycle

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

type Group struct{ wg sync.WaitGroup }

func (g *Group) Go(ctx context.Context, fn func(context.Context)) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		fn(ctx)
	}()
}

func (g *Group) Wait() { g.wg.Wait() }

func Recover(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: recovered from panic: %v", name, r)
		}
	}()
	fn()
}

func Every(ctx context.Context, d time.Duration, now bool, name string, fn func(context.Context)) {
	if now {
		Recover(name, func() { fn(ctx) })
	}
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			Recover(name, func() { fn(ctx) })
		}
	}
}

func CleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

func LogCleanup(what string, err error) {
	if err != nil {
		log.Printf("cleanup %s: %v", what, err)
	}
}

func Serve(hs *http.Server, run ...func(context.Context)) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	var g Group
	for _, r := range run {
		g.Go(ctx, r)
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServeTLS("", "") }()
	var err error
	select {
	case err = <-errc:
		stop()
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		err = hs.Shutdown(sctx)
	}
	done := make(chan struct{})
	go func() {
		g.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		log.Printf("background work did not stop in time")
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
