package managerserver

import (
	"net/http"
	"time"

	"argus/internal/auth"
	"argus/internal/ui"
)

type v1Event struct {
	Host    string    `json:"host"`
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	Object  string    `json:"object"`
}

type v1Pool struct {
	Host        string `json:"host"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Free        int64  `json:"freeBytes"`
	Used        int64  `json:"usedBytes"`
	Provisioned int64  `json:"provisionedBytes"`
	Volumes     int    `json:"volumes"`
	Snapshots   int    `json:"snapshots"`
}

type v1Volume struct {
	Host      string    `json:"host"`
	Name      string    `json:"name"`
	Size      int64     `json:"sizeBytes"`
	Phase     string    `json:"phase"`
	Progress  string    `json:"progress,omitempty"`
	Created   time.Time `json:"created"`
	Image     bool      `json:"image"`
	UsedBy    string    `json:"usedBy,omitempty"`
	Snapshots []string  `json:"snapshots,omitempty"`
	Thick     bool      `json:"thick"`
}

func (s *Server) v1ReadRoutes() []ui.Route {
	return []ui.Route{
		{Pattern: "GET /api/v1/events", Priv: auth.VMView, H: s.v1Events},
		{Pattern: "GET /api/v1/storage", Priv: auth.VMView, H: s.v1Storage},
		{Pattern: "GET /api/v1/firewall", Priv: auth.NetworkConfig, H: s.v1Firewall},
		{Pattern: "GET /api/v1/alarm-rules", Priv: auth.VMView, H: s.v1AlarmRules},
		{Pattern: "GET /api/v1/profile", Priv: auth.Administer, H: s.v1Profile},
	}
}

func v1Result(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		ui.APIError(w, r, http.StatusBadGateway, err)
		return
	}
	ui.WriteJSON(w, v)
}

func (s *Server) v1Events(w http.ResponseWriter, r *http.Request) {
	evs, _ := s.fleetEvents(r)
	out := []v1Event{}
	for _, e := range evs {
		out = append(out, v1Event{e.Host, e.Time, e.Type, e.Reason, e.Message, e.Object})
	}
	ui.WriteJSON(w, out)
}

func (s *Server) v1Storage(w http.ResponseWriter, r *http.Request) {
	pools, vols, _ := s.fleetStorage(r)
	out := struct {
		Pools   []v1Pool   `json:"pools"`
		Volumes []v1Volume `json:"volumes"`
	}{[]v1Pool{}, []v1Volume{}}
	for _, p := range pools {
		out.Pools = append(out.Pools, v1Pool{p.Host, p.Name, p.Status, p.Free, p.Used, p.Provisioned, p.Volumes, p.Snapshots})
	}
	for _, v := range vols {
		out.Volumes = append(out.Volumes, v1Volume{v.Host, v.Name, v.Size, v.Phase, v.Progress, v.Created, v.Image, v.UsedBy, v.Snapshots, v.Thick})
	}
	ui.WriteJSON(w, out)
}

func (s *Server) v1Firewall(w http.ResponseWriter, r *http.Request) {
	p, err := s.manager.Firewall(r.Context(), ui.Session(r).Org)
	v1Result(w, r, p, err)
}

func (s *Server) v1AlarmRules(w http.ResponseWriter, r *http.Request) {
	rules, err := s.manager.AlarmRules(r.Context(), ui.Session(r).Org)
	v1Result(w, r, rules, err)
}

func (s *Server) v1Profile(w http.ResponseWriter, r *http.Request) {
	p, err := s.manager.Profile(r.Context(), ui.Session(r).Org, r.FormValue("dc"), r.FormValue("cluster"))
	v1Result(w, r, p, err)
}
