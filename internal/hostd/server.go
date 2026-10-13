package hostd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
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
	s.mux.HandleFunc("GET /disks/{name}", func(w http.ResponseWriter, r *http.Request) {
		dd, err := diskDetail(r.Context(), r.PathValue("name"))
		reply(w, dd, err)
	})
	s.mux.HandleFunc("POST /disks/{name}/{action}", s.locked(s.diskAction))
	s.mux.HandleFunc("GET /pools", s.pools)
	s.mux.HandleFunc("POST /pools", s.locked(s.create))
	s.mux.HandleFunc("POST /pools/{name}/add", s.locked(s.add))
	s.mux.HandleFunc("POST /pools/{name}/import", s.locked(s.importPool))
	s.mux.HandleFunc("POST /pools/{name}/{action}", s.locked(s.release))
	s.mux.HandleFunc("GET /network", s.network)
	s.mux.HandleFunc("GET /backup/files/{rel...}", s.backupFile)
	s.mux.HandleFunc("PUT /backup/files/{rel...}", s.backupFile)
	s.mux.HandleFunc("GET /backup/list", func(w http.ResponseWriter, r *http.Request) {
		bs, err := listBackups(r.Context())
		reply(w, bs, err)
	})
	s.mux.HandleFunc("POST /backup/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Path string }
		err := decode(r, &req)
		if err == nil {
			err = deleteBackup(r.Context(), req.Path)
		}
		reply(w, nil, err)
	})
	s.mux.HandleFunc("GET /zfs/snapshots", func(w http.ResponseWriter, r *http.Request) {
		ds := r.URL.Query()["ds"]
		err := checkDatasets(ds)
		out := map[string][]string{}
		for _, d := range ds {
			if err == nil {
				out[d], err = zfsSnapshots(r.Context(), d, r.URL.Query().Get("kind"))
			}
		}
		reply(w, out, err)
	})
	s.mux.HandleFunc("POST /zfs/snapshot", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Datasets []string
			Snap     string
		}
		err := decode(r, &req)
		if err == nil {
			err = checkDatasets(req.Datasets)
		}
		if err == nil && !validZSnap.MatchString(req.Snap) {
			err = fmt.Errorf("invalid snapshot %q", req.Snap)
		}
		if err == nil {
			err = zfsSnapshot(r.Context(), req.Datasets, req.Snap)
		}
		reply(w, nil, err)
	})
	s.mux.HandleFunc("POST /zfs/prune", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Datasets, Keep []string
			Kind           string
		}
		err := decode(r, &req)
		if err == nil {
			err = checkDatasets(req.Datasets)
		}
		if err == nil {
			err = zfsPrune(r.Context(), req.Datasets, req.Keep, req.Kind)
		}
		reply(w, nil, err)
	})
	s.mux.HandleFunc("POST /firewall", func(w http.ResponseWriter, r *http.Request) {
		var vms []FWVM
		err := decode(r, &vms)
		if err == nil {
			err = applyFirewall(r.Context(), vms)
		}
		reply(w, nil, err)
	})
	s.mux.HandleFunc("GET /zfs/send", zfsSend)
	s.mux.HandleFunc("GET /zfs/changes", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		ch, err := zfsChanges(r.Context(), q.Get("ds"), q.Get("snap"), q.Get("from"))
		reply(w, ch, err)
	})
	s.mux.HandleFunc("GET /zfs/read", zfsRead)
	s.mux.HandleFunc("PUT /flr/{id}/{disk}", func(w http.ResponseWriter, r *http.Request) {
		err := FLRLoad(r.Context(), r.PathValue("id"), r.PathValue("disk"), r.Body)
		log.Printf("file restore %s %s: %v", r.PathValue("id"), r.PathValue("disk"), err)
		reply(w, nil, err)
	})
	s.mux.HandleFunc("POST /flr/{id}/{disk}/attach", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Dataset string }
		err := decode(r, &req)
		if err == nil {
			err = FLRAttachVolume(r.Context(), r.PathValue("id"), r.PathValue("disk"), req.Dataset)
		}
		log.Printf("file browse %s %s: %v", r.PathValue("id"), req.Dataset, err)
		reply(w, nil, err)
	})
	s.mux.HandleFunc("GET /flr/{id}", func(w http.ResponseWriter, r *http.Request) {
		parts, err := FLRParts(r.PathValue("id"))
		reply(w, parts, err)
	})
	s.mux.HandleFunc("GET /flr/{id}/ls", func(w http.ResponseWriter, r *http.Request) {
		entries, err := FLRList(r.Context(), r.PathValue("id"), r.URL.Query().Get("part"), r.URL.Query().Get("path"))
		reply(w, entries, err)
	})
	s.mux.HandleFunc("GET /flr/{id}/file", func(w http.ResponseWriter, r *http.Request) {
		started := false
		err := FLRCopy(r.Context(), r.PathValue("id"), r.URL.Query().Get("part"), r.URL.Query().Get("path"), func(size int64) io.Writer {
			started = true
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			return w
		})
		if err != nil && !started {
			reply(w, nil, err)
		}
	})
	s.mux.HandleFunc("POST /flr/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, FLRClose(r.Context(), r.PathValue("id")))
	})
	s.mux.HandleFunc("POST /flr/sweep", func(w http.ResponseWriter, r *http.Request) {
		if closed := FLRSweep(r.Context(), 2*time.Hour); len(closed) > 0 {
			log.Printf("file restore sweep: %v", closed)
		}
		reply(w, nil, nil)
	})
	s.mux.HandleFunc("GET /zfs/volumes", func(w http.ResponseWriter, r *http.Request) {
		vols, err := ZVols(r.Context(), r.URL.Query()["ds"])
		reply(w, vols, err)
	})
	s.mux.HandleFunc("POST /zfs/provisioning", s.locked(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Dataset string
			Thick   bool
		}
		err := decode(r, &req)
		if err == nil {
			err = SetThick(r.Context(), req.Dataset, req.Thick)
		}
		log.Printf("provisioning %s thick=%v: %v", req.Dataset, req.Thick, err)
		reply(w, nil, err)
	}))
	s.mux.HandleFunc("POST /zfs/release", s.locked(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Dataset string }
		err := decode(r, &req)
		if err == nil {
			err = ReleaseVolume(r.Context(), req.Dataset)
		}
		log.Printf("release %s: %v", req.Dataset, err)
		reply(w, nil, err)
	}))
	s.mux.HandleFunc("POST /zfs/sweep", s.locked(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Keep []string }
		err := decode(r, &req)
		var gone []string
		if err == nil {
			gone, err = SweepSnapshots(r.Context(), req.Keep)
		}
		if len(gone) > 0 || err != nil {
			log.Printf("sweep snapshots %v: %v", gone, err)
		}
		reply(w, gone, err)
	}))
	s.mux.HandleFunc("POST /zfs/destroy", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Datasets []string
			Snap     string
		}
		err := decode(r, &req)
		if err == nil {
			err = checkDatasets(req.Datasets)
		}
		if err == nil {
			err = zfsDestroy(r.Context(), req.Datasets, req.Snap)
		}
		reply(w, nil, err)
	})
	s.mux.HandleFunc("PUT /zfs/recv", zfsRecv)
	s.mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) { reply(w, readSettings(), nil) })
	s.mux.HandleFunc("POST /settings", s.locked(func(w http.ResponseWriter, r *http.Request) {
		var u SettingsUpdate
		err := decode(r, &u)
		if err == nil {
			err = applySettings(r.Context(), u)
		}
		log.Printf("settings: %v", err)
		reply(w, nil, err)
	}))
	s.mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) { reply(w, hostStats(r.Context()), nil) })
	s.mux.HandleFunc("POST /network/bridges", s.locked(s.bridge))
	s.mux.HandleFunc("POST /network/bridges/{name}/delete", s.locked(s.unbridge))
	s.mux.HandleFunc("POST /network/bridges/{name}/teaming", s.locked(func(w http.ResponseWriter, r *http.Request) {
		var t Teaming
		err := decode(r, &t)
		if err == nil {
			err = s.setTeaming(r.Context(), r.PathValue("name"), t)
		}
		log.Printf("set teaming %s %+v: %v", r.PathValue("name"), t, err)
		reply(w, nil, err)
	}))
	s.mux.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		devs, err := ListDevices(r.Context())
		reply(w, devs, err)
	})
	s.mux.HandleFunc("POST /devices", s.locked(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Kind, ID string
			On       bool
		}
		err := decode(r, &req)
		if err == nil {
			err = SetPassthrough(r.Context(), req.Kind, req.ID, req.On)
		}
		log.Printf("passthrough %s %s %v: %v", req.Kind, req.ID, req.On, err)
		reply(w, nil, err)
	}))
	s.mux.HandleFunc("GET /network/bridges/{name}/ports", func(w http.ResponseWriter, r *http.Request) {
		ports, err := BridgePorts(r.Context(), r.PathValue("name"))
		reply(w, ports, err)
	})
	s.mux.HandleFunc("POST /network/address", s.locked(s.address))
	s.mux.HandleFunc("POST /network/management", s.locked(s.management))
	s.mux.HandleFunc("POST /network/confirm", s.locked(func(w http.ResponseWriter, r *http.Request) { reply(w, nil, Confirm(r.Context())) }))
	s.mux.HandleFunc("POST /network/rollback", s.locked(func(w http.ResponseWriter, r *http.Request) { reply(w, nil, Rollback(r.Context())) }))
	s.touch()
	return s
}

func (s *Server) touch() { s.last.Store(time.Now().Unix()) }

const (
	NodeIPHeader = "X-Argus-Node-IP"
	AccessHeader = "X-Argus-Access-Host"
)

type nodeIPKey struct{}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.active.Add(1)
	defer func() { s.active.Add(-1); s.touch() }()
	ips := []string{r.Header.Get(NodeIPHeader), r.Header.Get(AccessHeader)}
	s.mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nodeIPKey{}, ips)))
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
	err := importPool(r.Context(), r.PathValue("name"), r.URL.Query().Get("force") == "1")
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

func (s *Server) network(w http.ResponseWriter, r *http.Request) {
	hn, err := hostNet(r.Context())
	reply(w, hn, err)
}

func (s *Server) bridge(w http.ResponseWriter, r *http.Request) {
	var spec BridgeSpec
	err := decode(r, &spec)
	if err == nil {
		err = s.createBridge(r.Context(), spec)
	}
	log.Printf("create bridge %s: %v", spec.Name, err)
	reply(w, nil, err)
}

func (s *Server) unbridge(w http.ResponseWriter, r *http.Request) {
	err := s.deleteBridge(r.Context(), r.PathValue("name"))
	log.Printf("delete bridge %s: %v", r.PathValue("name"), err)
	reply(w, nil, err)
}

func (s *Server) address(w http.ResponseWriter, r *http.Request) {
	var a AddressSpec
	err := decode(r, &a)
	if err == nil {
		err = s.setAddress(r.Context(), a)
	}
	log.Printf("set address %s: %v", a.Iface, err)
	reply(w, nil, err)
}

func (s *Server) disks(w http.ResponseWriter, r *http.Request) {
	ds, err := listDisks(r.Context())
	reply(w, ds, err)
}

func (s *Server) management(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Iface string `json:"iface"`
		On    bool   `json:"on"`
	}
	err := decode(r, &req)
	if err == nil {
		err = s.setManagement(r.Context(), req.Iface, req.On)
	}
	log.Printf("management role %s=%v: %v", req.Iface, req.On, err)
	reply(w, nil, err)
}

func (s *Server) diskAction(w http.ResponseWriter, r *http.Request) {
	var req struct{ Confirm, Test string }
	decode(r, &req)
	name, action := r.PathValue("name"), r.PathValue("action")
	var err error
	switch action {
	case "wipe":
		err = wipeDisk(r.Context(), name, req.Confirm)
	case "smart-test":
		err = smartTest(r.Context(), name, req.Test)
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	log.Printf("disk %s %s: %v", name, action, err)
	reply(w, nil, err)
}
