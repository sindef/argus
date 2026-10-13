package ui

import (
	"bytes"
	"cmp"
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

func (s *Core) LoadNotify(ctx context.Context) (Notify, string, error) {
	var n Notify
	rv, err := s.LoadJSON(ctx, notifyKey, &n)
	return n, rv, err
}

func (s *Core) SaveNotify(ctx context.Context, n Notify) error {
	old, rv, err := s.LoadNotify(ctx)
	if err != nil {
		return err
	}
	if n.SMTPPass == "" {
		n.SMTPPass = old.SMTPPass
	}
	return s.SaveJSON(ctx, notifyKey, rv, n)
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

func (s *Core) ApplyAlarms(found []Alarm) {
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
		n, _, err := s.LoadNotify(ctx)
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

func (s *Core) ActiveAlarms() []Alarm {
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

func (s *Core) notifyForm(w http.ResponseWriter, r *http.Request) {
	if !s.Mode.Super(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	n := Notify{Webhook: strings.TrimSpace(r.FormValue("webhook")), SMTPHost: strings.TrimSpace(r.FormValue("smtp_host")), SMTPUser: strings.TrimSpace(r.FormValue("smtp_user")), SMTPPass: r.FormValue("smtp_pass"), From: strings.TrimSpace(r.FormValue("from")), To: strings.TrimSpace(r.FormValue("to"))}
	if n.Webhook != "" && !strings.HasPrefix(n.Webhook, "https://") && !strings.HasPrefix(n.Webhook, "http://") {
		Flash(w, r, errors.New("webhook must be an http(s) URL"))
		return
	}
	err := s.SaveNotify(r.Context(), n)
	if err == nil && r.FormValue("test") != "" {
		n, _, err = s.LoadNotify(r.Context())
		if err == nil {
			err = n.send(r.Context(), "Argus test notification", "Test notification from "+cmp.Or(s.NodeName, s.Product))
		}
	}
	if err != nil {
		Flash(w, r, err)
		return
	}
	w.Header().Set("HX-Refresh", "true")
}
