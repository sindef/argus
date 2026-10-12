package hostd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	resolvedConf  = "/etc/systemd/resolved.conf.d/argus.conf"
	timesyncdConf = "/etc/systemd/timesyncd.conf.d/argus.conf"
	syslogConf    = "/usr/local/argus/rsyslog.d/remote.conf"
	unsignedFile  = "/usr/local/argus/allow-unsigned-updates"
)

type Settings struct {
	DNS      []string `json:"dns"`
	NTP      []string `json:"ntp"`
	Syslog   string   `json:"syslog"`
	Backup   string   `json:"backup"`
	Unsigned bool     `json:"unsignedUpdates"`
}

type SettingsUpdate struct {
	DNS      *[]string `json:"dns,omitempty"`
	NTP      *[]string `json:"ntp,omitempty"`
	Syslog   *string   `json:"syslog,omitempty"`
	Backup   *string   `json:"backup,omitempty"`
	Unsigned *bool     `json:"unsignedUpdates,omitempty"`
}

var (
	validHost   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)
	syslogLine  = regexp.MustCompile(`^\*\.\* (@@?)(\S+):(\d+)$`)
	syslogValue = regexp.MustCompile(`^(udp|tcp)://([^/]+):(\d+)$`)
)

func confValues(path, key string) []string {
	b, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.Fields(v)
		}
	}
	return nil
}

func readSettings() Settings {
	_, err := os.Stat(unsignedFile)
	st := Settings{DNS: confValues(resolvedConf, "DNS"), NTP: confValues(timesyncdConf, "NTP"), Backup: backupTarget(), Unsigned: err == nil}
	b, _ := os.ReadFile(syslogConf)
	if m := syslogLine.FindStringSubmatch(strings.TrimSpace(string(b))); m != nil {
		proto := "udp"
		if m[1] == "@@" {
			proto = "tcp"
		}
		st.Syslog = proto + "://" + m[2] + ":" + m[3]
	}
	return st
}

func writeConf(path, body string) error {
	if body == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func applySettings(ctx context.Context, u SettingsUpdate) error {
	if u.DNS != nil {
		for _, d := range *u.DNS {
			if net.ParseIP(d) == nil {
				return fmt.Errorf("invalid DNS server %q", d)
			}
		}
		body := ""
		if len(*u.DNS) > 0 {
			body = "[Resolve]\nDNS=" + strings.Join(*u.DNS, " ") + "\n"
		}
		if err := writeConf(resolvedConf, body); err != nil {
			return err
		}
		if _, err := run(ctx, "systemctl", "restart", "systemd-resolved"); err != nil {
			return err
		}
	}
	if u.NTP != nil {
		for _, n := range *u.NTP {
			if net.ParseIP(n) == nil && !validHost.MatchString(n) {
				return fmt.Errorf("invalid NTP server %q", n)
			}
		}
		body := ""
		if len(*u.NTP) > 0 {
			body = "[Time]\nNTP=" + strings.Join(*u.NTP, " ") + "\n"
		}
		if err := writeConf(timesyncdConf, body); err != nil {
			return err
		}
		if _, err := run(ctx, "systemctl", "restart", "systemd-timesyncd"); err != nil {
			return err
		}
	}
	if u.Unsigned != nil {
		body := ""
		if *u.Unsigned {
			body = "1\n"
		}
		if err := writeConf(unsignedFile, body); err != nil {
			return err
		}
	}
	if u.Backup != nil {
		if err := setBackupTarget(ctx, strings.TrimSpace(*u.Backup)); err != nil {
			return err
		}
	}
	if u.Syslog != nil {
		body := ""
		if *u.Syslog != "" {
			m := syslogValue.FindStringSubmatch(*u.Syslog)
			if m == nil {
				return errors.New("syslog target must look like udp://host:514 or tcp://host:514")
			}
			if port, _ := strconv.Atoi(m[3]); port < 1 || port > 65535 || (net.ParseIP(strings.Trim(m[2], "[]")) == nil && !validHost.MatchString(m[2])) {
				return errors.New("syslog target must look like udp://host:514 or tcp://host:514")
			}
			at := "@"
			if m[1] == "tcp" {
				at = "@@"
			}
			body = "*.* " + at + m[2] + ":" + m[3] + "\n"
		}
		_, missing := os.Stat("/usr/sbin/rsyslogd")
		if missing != nil && body != "" {
			return errors.New("rsyslog is not installed on this host")
		}
		if err := writeConf(syslogConf, body); err != nil {
			return err
		}
		if missing == nil {
			if _, err := run(ctx, "systemctl", "restart", "rsyslog"); err != nil {
				return err
			}
		}
	}
	return nil
}
