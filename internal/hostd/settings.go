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
		if err := setServers(ctx, *u.DNS, "DNS", resolvedConf, "[Resolve]\nDNS=", "systemd-resolved", func(v string) bool { return net.ParseIP(v) != nil }); err != nil {
			return err
		}
	}
	if u.NTP != nil {
		if err := setServers(ctx, *u.NTP, "NTP", timesyncdConf, "[Time]\nNTP=", "systemd-timesyncd", func(v string) bool { return net.ParseIP(v) != nil || validHost.MatchString(v) }); err != nil {
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
		return setSyslog(ctx, *u.Syslog)
	}
	return nil
}

func setServers(ctx context.Context, servers []string, kind, file, prefix, unit string, valid func(string) bool) error {
	for _, v := range servers {
		if !valid(v) {
			return fmt.Errorf("invalid %s server %q", kind, v)
		}
	}
	body := ""
	if len(servers) > 0 {
		body = prefix + strings.Join(servers, " ") + "\n"
	}
	if err := writeConf(file, body); err != nil {
		return err
	}
	_, err := run(ctx, "systemctl", "restart", unit)
	return err
}

func setSyslog(ctx context.Context, target string) error {
	body := ""
	if target != "" {
		m := syslogValue.FindStringSubmatch(target)
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
		_, err := run(ctx, "systemctl", "restart", "rsyslog")
		return err
	}
	return nil
}
