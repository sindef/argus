package hostd

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type Server struct {
	mux    *http.ServeMux
	mu     sync.Mutex
	active atomic.Int64
	last   atomic.Int64
}

func NewServer() *Server {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /disks", s.disks)
	s.mux.HandleFunc("GET /pools", s.pools)
	s.mux.HandleFunc("POST /pools", s.locked(s.create))
	s.mux.HandleFunc("POST /pools/{name}/add", s.locked(s.add))
	s.mux.HandleFunc("POST /pools/{name}/import", s.locked(s.importPool))
	s.mux.HandleFunc("POST /pools/{name}/{action}", s.locked(s.release))
	s.touch()
	return s
}

func (s *Server) touch() { s.last.Store(time.Now().Unix()) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.active.Add(1)
	defer func() { s.active.Add(-1); s.touch() }()
	s.mux.ServeHTTP(w, r)
}

func (s *Server) Idle(d time.Duration) bool {
	return s.active.Load() == 0 && time.Since(time.Unix(s.last.Load(), 0)) > d
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		log.Printf("%v", err)
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if v == nil {
		v = map[string]string{}
	}
	json.NewEncoder(w).Encode(v)
}

func (s *Server) locked(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		h(w, r)
	}
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("invalid request: %v", err)
	}
	return nil
}

func (s *Server) pools(w http.ResponseWriter, r *http.Request) {
	ps, err := listPools(r.Context())
	reply(w, ps, err)
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var spec PoolSpec
	err := decode(r, &spec)
	if err == nil {
		err = s.createPool(r.Context(), spec)
	}
	log.Printf("create pool %s: %v", spec.Name, err)
	reply(w, nil, err)
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	var spec PoolSpec
	err := decode(r, &spec)
	if err == nil {
		err = s.addToPool(r.Context(), r.PathValue("name"), spec)
	}
	log.Printf("add to pool %s: %v", r.PathValue("name"), err)
	reply(w, nil, err)
}

func (s *Server) importPool(w http.ResponseWriter, r *http.Request) {
	err := importPool(r.Context(), r.PathValue("name"))
	log.Printf("import pool %s: %v", r.PathValue("name"), err)
	reply(w, nil, err)
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action != "export" && action != "destroy" {
		reply(w, nil, fmt.Errorf("unknown action %q", action))
		return
	}
	var req struct{ Confirm string }
	decode(r, &req)
	err := releasePool(r.Context(), r.PathValue("name"), action, req.Confirm)
	log.Printf("%s pool %s: %v", action, r.PathValue("name"), err)
	reply(w, nil, err)
}

func (s *Server) disks(w http.ResponseWriter, r *http.Request) {
	ds, err := listDisks(r.Context())
	reply(w, ds, err)
}
