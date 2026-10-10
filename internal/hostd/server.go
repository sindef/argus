package hostd

import (
	"encoding/json"
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

func (s *Server) disks(w http.ResponseWriter, r *http.Request) {
	ds, err := listDisks(r.Context())
	reply(w, ds, err)
}
