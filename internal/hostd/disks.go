package hostd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type lsblkDev struct {
	Name        string     `json:"name"`
	Path        string     `json:"path"`
	Type        string     `json:"type"`
	Fstype      string     `json:"fstype"`
	Label       string     `json:"label"`
	Pttype      string     `json:"pttype"`
	Tran        string     `json:"tran"`
	Model       string     `json:"model"`
	Serial      string     `json:"serial"`
	Size        int64      `json:"size"`
	Rota        bool       `json:"rota"`
	Mountpoints []string   `json:"mountpoints"`
	Children    []lsblkDev `json:"children"`
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

var idPreference = []string{"wwn-", "nvme-eui.", "nvme-", "ata-", "scsi-", "virtio-", "usb-"}

func diskIDs() map[string]string {
	best := map[string]string{}
	rank := map[string]int{}
	entries, _ := os.ReadDir("/dev/disk/by-id")
	for _, e := range entries {
		if strings.Contains(e.Name(), "-part") {
			continue
		}
		target, err := filepath.EvalSymlinks(filepath.Join("/dev/disk/by-id", e.Name()))
		if err != nil {
			continue
		}
		for i, p := range idPreference {
			if strings.HasPrefix(e.Name(), p) {
				if cur, ok := rank[target]; !ok || i < cur {
					best[target], rank[target] = "/dev/disk/by-id/"+e.Name(), i
				}
				break
			}
		}
	}
	return best
}

func poolMembers(ctx context.Context) map[string]string {
	out, err := run(ctx, "zpool", "status", "-LP")
	if err != nil {
		return nil
	}
	members := map[string]string{}
	pool := ""
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "pool:" && len(f) > 1 {
			pool = f[1]
		} else if strings.HasPrefix(f[0], "/dev/") {
			members[f[0]] = pool
		}
	}
	return members
}

func virtualDisk(name string) bool {
	for _, p := range []string{"zd", "nbd", "rbd", "drbd", "ram", "zram"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func walk(d lsblkDev, fn func(lsblkDev)) {
	fn(d)
	for _, c := range d.Children {
		walk(c, fn)
	}
}

func listDisks(ctx context.Context) ([]Disk, error) {
	out, err := run(ctx, "lsblk", "-J", "-b", "-o", "NAME,PATH,TYPE,FSTYPE,LABEL,PTTYPE,TRAN,MODEL,SERIAL,SIZE,ROTA,MOUNTPOINTS")
	if err != nil {
		return nil, err
	}
	var tree struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(out), &tree); err != nil {
		return nil, err
	}
	ids := diskIDs()
	members := poolMembers(ctx)
	var disks []Disk
	for _, b := range tree.Blockdevices {
		if b.Type != "disk" || b.Size == 0 || virtualDisk(b.Name) {
			continue
		}
		d := Disk{Name: b.Name, Path: b.Path, ID: ids[b.Path], Model: strings.TrimSpace(b.Model), Serial: strings.TrimSpace(b.Serial), Transport: b.Tran, Size: b.Size}
		if d.ID == "" {
			d.ID = b.Path
		}
		switch {
		case b.Tran == "nvme":
			d.Media = "nvme"
		case b.Rota:
			d.Media = "hdd"
		default:
			d.Media = "ssd"
		}
		if b.Pttype != "" {
			d.Signatures = append(d.Signatures, b.Pttype+" partition table")
		}
		walk(b, func(x lsblkDev) {
			if strings.HasPrefix(x.Label, "COS_") {
				d.Use = "os"
			}
			for _, m := range x.Mountpoints {
				if m != "" && d.Use == "" {
					d.Use = "mounted"
				}
			}
			if p, ok := members[x.Path]; ok {
				d.Pool = p
			}
			switch {
			case x.Fstype == "zfs_member":
				d.Signatures = append(d.Signatures, "ZFS pool "+x.Label)
			case x.Fstype != "":
				d.Signatures = append(d.Signatures, strings.TrimSpace(x.Fstype+" "+x.Label))
			}
		})
		if d.Pool != "" && d.Use == "" {
			d.Use = "pool"
		}
		disks = append(disks, d)
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Name < disks[j].Name })
	return disks, nil
}
