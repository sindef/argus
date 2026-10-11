package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"sort"
	"strings"
	"sync"
	"time"

	"argus/internal/manager"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type Alarm struct {
	Key      string    `json:"key"`
	Severity string    `json:"severity"`
	Host     string    `json:"host"`
	Object   string    `json:"object"`
	Message  string    `json:"message"`
	Since    time.Time `json:"since"`
}

type alarmState struct {
	mu       sync.Mutex
	active   map[string]Alarm
	smart    map[string]Alarm
	smartAt  time.Time
	notifyMu sync.Mutex
}

type Notify struct {
	Webhook  string `json:"webhook"`
	SMTPHost string `json:"smtpHost"`
	SMTPUser string `json:"smtpUser"`
	SMTPPass string `json:"smtpPass"`
	From     string `json:"from"`
	To       string `json:"to"`
}

const notifyKey = "argus-notify"

func (s *Server) loadNotify(ctx context.Context) (Notify, string, error) {
	var n Notify
	data, rv, err := s.auth.Secrets().Get(ctx, notifyKey)
	if apierrors.IsNotFound(err) {
		return n, "", nil
	}
	if err != nil {
		return n, "", err
	}
	return n, rv, json.Unmarshal(data["config"], &n)
}

func (s *Server) saveNotify(ctx context.Context, n Notify) error {
	old, rv, err := s.loadNotify(ctx)
	if err != nil {
		return err
	}
	if n.SMTPPass == "" {
		n.SMTPPass = old.SMTPPass
	}
	b, _ := json.Marshal(n)
	data := map[string][]byte{"config": b}
	if rv == "" {
		return s.auth.Secrets().Create(ctx, notifyKey, "Opaque", data)
	}
	return s.auth.Secrets().Update(ctx, notifyKey, rv, data)
}

func (n Notify) send(ctx context.Context, subject, text string) error {
	var errs []string
	if n.Webhook != "" {
		b, _ := json.Marshal(map[string]string{"text": text, "content": text})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.Webhook, bytes.NewReader(b))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			var resp *http.Response
			if resp, err = (&http.Client{Timeout: 15 * time.Second}).Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode >= 300 {
					err = fmt.Errorf("webhook returned %s", resp.Status)
				}
			}
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if n.SMTPHost != "" && n.To != "" {
		host, _, err := net.SplitHostPort(n.SMTPHost)
		if err != nil {
			host = n.SMTPHost
			n.SMTPHost += ":587"
		}
		var a smtp.Auth
		if n.SMTPUser != "" {
			a = smtp.PlainAuth("", n.SMTPUser, n.SMTPPass, host)
		}
		from := n.From
		if from == "" {
			from = n.SMTPUser
		}
		msg := "From: " + from + "\r\nTo: " + n.To + "\r\nSubject: " + subject + "\r\n\r\n" + text + "\r\n"
		if err := smtp.SendMail(n.SMTPHost, a, from, strings.Split(n.To, ","), []byte(msg)); err != nil {
			errs = append(errs, "email: "+err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (s *Server) applyAlarms(found []Alarm) {
	s.alarms.mu.Lock()
	if s.alarms.active == nil {
		s.alarms.active = map[string]Alarm{}
	}
	next := map[string]Alarm{}
	var raised, cleared []Alarm
	for _, a := range found {
		if old, ok := s.alarms.active[a.Key]; ok {
			a.Since = old.Since
		} else {
			a.Since = time.Now()
			raised = append(raised, a)
		}
		next[a.Key] = a
	}
	for k, a := range s.alarms.active {
		if _, ok := next[k]; !ok {
			cleared = append(cleared, a)
		}
	}
	s.alarms.active = next
	s.alarms.mu.Unlock()
	if len(raised)+len(cleared) == 0 {
		return
	}
	var lines []string
	for _, a := range raised {
		lines = append(lines, fmt.Sprintf("[%s] %s", strings.ToUpper(a.Severity), a.text()))
	}
	for _, a := range cleared {
		lines = append(lines, "[RESOLVED] "+a.text())
	}
	go func() {
		s.alarms.notifyMu.Lock()
		defer s.alarms.notifyMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		n, _, err := s.loadNotify(ctx)
		if err == nil {
			err = n.send(ctx, "Argus alarms", strings.Join(lines, "\n"))
		}
		if err != nil {
			log.Printf("alarms: notify: %v", err)
		}
	}()
}

func (a Alarm) text() string {
	if a.Host != "" {
		return a.Host + ": " + a.Object + ": " + a.Message
	}
	return a.Object + ": " + a.Message
}

func (s *Server) activeAlarms() []Alarm {
	s.alarms.mu.Lock()
	defer s.alarms.mu.Unlock()
	out := make([]Alarm, 0, len(s.alarms.active))
	for _, a := range s.alarms.active {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == "critical"
		}
		return out[i].Since.After(out[j].Since)
	})
	return out
}

func (s *Server) alarmLoop() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if s.manager != nil {
			s.applyAlarms(s.fleetAlarms(ctx))
		} else {
			s.applyAlarms(s.nodeAlarms(ctx))
		}
		cancel()
		time.Sleep(time.Minute)
	}
}

func (s *Server) nodeAlarms(ctx context.Context) []Alarm {
	var out []Alarm
	add := func(key, sev, object, msg string) {
		out = append(out, Alarm{Key: key, Severity: sev, Object: object, Message: msg})
	}
	if s.hostd != nil {
		if ps, err := s.hostd.Pools(ctx); err == nil {
			for _, p := range ps.Pools {
				if p.Health != "ONLINE" {
					add("pool-health:"+p.Name, "critical", "pool "+p.Name, "health is "+p.Health)
				}
				if p.Size > 0 {
					switch pct := p.Alloc * 100 / p.Size; {
					case pct >= 90:
						add("pool-full:"+p.Name, "critical", "pool "+p.Name, fmt.Sprintf("%d%% used", pct))
					case pct >= 80:
						add("pool-full:"+p.Name, "warning", "pool "+p.Name, fmt.Sprintf("%d%% used", pct))
					}
				}
			}
		}
		if st, err := s.hostd.Stats(ctx); err == nil && st.MemTotal > 0 && st.MemAvail*10 < st.MemTotal {
			add("memory", "warning", "host", fmt.Sprintf("%d%% memory available", st.MemAvail*100/st.MemTotal))
		}
		out = append(out, s.smartAlarms(ctx)...)
	}
	s.backups.mu.Lock()
	for vm, last := range s.backups.last {
		if strings.HasPrefix(last, "Failed") {
			add("backup:"+vm, "warning", "VM "+vm, "last "+strings.ToLower(last))
		}
	}
	s.backups.mu.Unlock()
	if vms, err := s.kube.ListVMs(ctx); err == nil {
		for _, v := range vms {
			switch v.Status {
			case "ErrorUnschedulable", "CrashLoopBackOff", "DataVolumeError", "ErrImagePull", "ErrorPvcNotFound":
				add("vm:"+v.Namespace+"/"+v.Name, "warning", "VM "+v.Name, v.Status)
			}
		}
	}
	return out
}

func (s *Server) smartAlarms(ctx context.Context) []Alarm {
	s.alarms.mu.Lock()
	if time.Since(s.alarms.smartAt) < time.Hour {
		cached := s.alarms.smart
		s.alarms.mu.Unlock()
		out := make([]Alarm, 0, len(cached))
		for _, a := range cached {
			out = append(out, a)
		}
		return out
	}
	s.alarms.mu.Unlock()
	found := map[string]Alarm{}
	if disks, err := s.hostd.Disks(ctx); err == nil {
		for _, d := range disks {
			dd, err := s.hostd.Disk(ctx, d.Name)
			if err == nil && dd.Smart.Supported && !dd.Smart.Passed {
				found[d.Name] = Alarm{Key: "smart:" + d.Name, Severity: "critical", Object: "disk " + d.Name, Message: "SMART health check failed"}
			}
		}
	}
	s.alarms.mu.Lock()
	s.alarms.smart, s.alarms.smartAt = found, time.Now()
	s.alarms.mu.Unlock()
	out := make([]Alarm, 0, len(found))
	for _, a := range found {
		out = append(out, a)
	}
	return out
}

func (s *Server) fleetAlarms(ctx context.Context) []Alarm {
	var (
		mu  sync.Mutex
		out []Alarm
	)
	nodes, _ := s.manager.Nodes(ctx)
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n manager.Node) {
			defer wg.Done()
			var as []Alarm
			err := n.Call(ctx, http.MethodGet, "/api/alarms", nil, &as)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !n.Maintenance {
					out = append(out, Alarm{Key: "host:" + n.Name, Severity: "critical", Host: n.Name, Object: "host", Message: "unreachable: " + err.Error()})
				}
				return
			}
			for _, a := range as {
				a.Key, a.Host = n.Name+":"+a.Key, n.Name
				out = append(out, a)
			}
		}(n)
	}
	wg.Wait()
	auto := s.manager.HASettings(ctx).Automatic
	if ps, err := s.manager.Protected(ctx); err == nil {
		for _, p := range ps {
			if !auto && s.manager.Down(p.Owner) && !p.LastSync.IsZero() {
				out = append(out, Alarm{Key: "ha-down:" + p.VM, Severity: "critical", Host: p.Owner, Object: "VM " + p.VM, Message: "host down; fail over manually to " + p.Replica + " once it is confirmed off"})
			}
			if p.LastError != "" {
				out = append(out, Alarm{Key: "ha:" + p.VM, Severity: "warning", Host: p.Owner, Object: "VM " + p.VM, Message: "replication: " + p.LastError})
			}
		}
	}
	return out
}

func (s *Server) apiAlarms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.activeAlarms())
}

func (s *Server) notifyForm(w http.ResponseWriter, r *http.Request) {
	if s.manager != nil && !superAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	n := Notify{Webhook: strings.TrimSpace(r.FormValue("webhook")), SMTPHost: strings.TrimSpace(r.FormValue("smtp_host")), SMTPUser: strings.TrimSpace(r.FormValue("smtp_user")), SMTPPass: r.FormValue("smtp_pass"), From: strings.TrimSpace(r.FormValue("from")), To: strings.TrimSpace(r.FormValue("to"))}
	if n.Webhook != "" && !strings.HasPrefix(n.Webhook, "https://") && !strings.HasPrefix(n.Webhook, "http://") {
		flash(w, r, errors.New("webhook must be an http(s) URL"))
		return
	}
	err := s.saveNotify(r.Context(), n)
	if err == nil && r.FormValue("test") != "" {
		n, _, err = s.loadNotify(r.Context())
		if err == nil {
			from := s.nodeName
			if s.manager != nil {
				from = "Argus Manager"
			}
			err = n.send(r.Context(), "Argus test notification", "Test notification from "+from)
		}
	}
	if err != nil {
		flash(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}
