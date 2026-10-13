package hostd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	passthroughFile = "/usr/local/argus/passthrough.json"
	pciDir          = "/sys/bus/pci/devices"
)

var (
	validPCI = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	validUSB = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{4}$`)
)

type PCIDevice struct {
	Address     string `json:"address"`
	Vendor      string `json:"vendor"`
	Device      string `json:"device"`
	Class       string `json:"class"`
	Name        string `json:"name"`
	Driver      string `json:"driver"`
	IOMMUGroup  string `json:"iommuGroup"`
	Passthrough bool   `json:"passthrough"`
	Blocked     string `json:"blocked,omitempty"`
}

type USBDevice struct {
	Port        string `json:"port"`
	Vendor      string `json:"vendor"`
	Product     string `json:"product"`
	Name        string `json:"name"`
	Passthrough bool   `json:"passthrough"`
}

type Devices struct {
	IOMMU bool        `json:"iommu"`
	PCI   []PCIDevice `json:"pci"`
	USB   []USBDevice `json:"usb"`
}

type passthrough struct {
	PCI []string `json:"pci"`
	USB []string `json:"usb"`
}

func readPassthrough() (passthrough, error) {
	var p passthrough
	b, err := os.ReadFile(passthroughFile)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	if err != nil {
		return p, fmt.Errorf("%s: %w", passthroughFile, err)
	}
	return p, nil
}

func sysfs(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimPrefix(strings.TrimSpace(string(b)), "0x")
}

func linkBase(path string) string {
	t, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

func pciNames() map[string]string {
	names := map[string]string{}
	f, err := os.Open("/usr/share/misc/pci.ids")
	if err != nil {
		return names
	}
	defer f.Close()
	vendor := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "C "):
			return names
		case line == "" || line[0] == '#':
		case line[0] != '\t':
			id, name, _ := strings.Cut(line, "  ")
			vendor, names[id] = id, name
		case !strings.HasPrefix(line, "\t\t"):
			id, name, _ := strings.Cut(strings.TrimPrefix(line, "\t"), "  ")
			names[vendor+":"+id] = name
		}
	}
	return names
}

func inUse(ctx context.Context, ips []string) (map[string]string, error) {
	busy := map[string]string{}
	hn, err := hostNet(ctx, ips)
	if err != nil {
		return nil, err
	}
	for _, i := range hn.Ifaces {
		if i.Kind == "ether" && (i.Master != "" || len(i.Addresses) > 0) {
			if dev, err := filepath.EvalSymlinks(filepath.Join("/sys/class/net", i.Name, "device")); err == nil {
				busy[dev] = "network port " + i.Name + " is in use"
			}
		}
	}
	disks, err := listDisks(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range disks {
		if !d.Free() {
			if dev, err := filepath.EvalSymlinks(filepath.Join("/sys/class/block", d.Name, "device")); err == nil {
				busy[dev] = "disk " + d.Name + " is in use"
			}
		}
	}
	return busy, nil
}

func ListDevices(ctx context.Context, ips []string) (Devices, error) {
	var out Devices
	groups, _ := os.ReadDir("/sys/kernel/iommu_groups")
	out.IOMMU = len(groups) > 0
	busy, err := inUse(ctx, ips)
	if err != nil {
		return out, err
	}
	enabled, err := readPassthrough()
	if err != nil {
		return out, err
	}
	names := pciNames()
	entries, _ := os.ReadDir(pciDir)
	for _, e := range entries {
		dir := filepath.Join(pciDir, e.Name())
		d := PCIDevice{Address: e.Name(), Vendor: sysfs(dir + "/vendor"), Device: sysfs(dir + "/device"), Class: sysfs(dir + "/class"),
			Driver: linkBase(dir + "/driver"), IOMMUGroup: linkBase(dir + "/iommu_group")}
		if strings.HasPrefix(d.Class, "06") {
			continue
		}
		d.Name = strings.TrimSpace(names[d.Vendor] + " " + names[d.Vendor+":"+d.Device])
		d.Passthrough = slices.Contains(enabled.PCI, d.Address)
		if d.IOMMUGroup == "" {
			d.Blocked = "no IOMMU group; enable VT-d or AMD-Vi"
		}
		real, _ := filepath.EvalSymlinks(dir)
		for dev, why := range busy {
			if dev == real || strings.HasPrefix(dev, real+"/") {
				d.Blocked = why
			}
		}
		out.PCI = append(out.PCI, d)
	}
	usb, _ := filepath.Glob("/sys/bus/usb/devices/*")
	for _, dir := range usb {
		v := sysfs(dir + "/idVendor")
		if v == "" || sysfs(dir+"/bDeviceClass") == "09" {
			continue
		}
		u := USBDevice{Port: filepath.Base(dir), Vendor: v, Product: sysfs(dir + "/idProduct"),
			Name: strings.TrimSpace(sysfs(dir+"/manufacturer") + " " + sysfs(dir+"/product"))}
		u.Passthrough = slices.Contains(enabled.USB, u.Vendor+":"+u.Product)
		out.USB = append(out.USB, u)
	}
	return out, nil
}

func bindVFIO(ctx context.Context, addr string, on bool) error {
	dir := filepath.Join(pciDir, addr)
	override := "\n"
	if on {
		if _, err := run(ctx, "modprobe", "vfio-pci"); err != nil {
			return err
		}
		override = "vfio-pci"
	}
	if err := os.WriteFile(dir+"/driver_override", []byte(override), 0o200); err != nil {
		return err
	}
	if drv := linkBase(dir + "/driver"); drv != "" && (drv == "vfio-pci") != on {
		if err := os.WriteFile(dir+"/driver/unbind", []byte(addr), 0o200); err != nil {
			return err
		}
	}
	return os.WriteFile("/sys/bus/pci/drivers_probe", []byte(addr), 0o200)
}

func SetPassthrough(ctx context.Context, ips []string, kind, id string, on bool) error {
	p, err := readPassthrough()
	if err != nil {
		return err
	}
	switch kind {
	case "pci":
		if !validPCI.MatchString(id) {
			return errors.New("invalid PCI address")
		}
		if on {
			devs, err := ListDevices(ctx, ips)
			if err != nil {
				return err
			}
			i := slices.IndexFunc(devs.PCI, func(d PCIDevice) bool { return d.Address == id })
			if i < 0 {
				return fmt.Errorf("PCI device %s not found", id)
			}
			if devs.PCI[i].Blocked != "" {
				return fmt.Errorf("%s cannot be passed through: %s", id, devs.PCI[i].Blocked)
			}
		}
		if err := bindVFIO(ctx, id, on); err != nil {
			return err
		}
		p.PCI = toggle(p.PCI, id, on)
	case "usb":
		if !validUSB.MatchString(id) {
			return errors.New("invalid USB vendor:product")
		}
		p.USB = toggle(p.USB, id, on)
	default:
		return errors.New("kind must be pci or usb")
	}
	b, _ := json.Marshal(p)
	return os.WriteFile(passthroughFile, b, 0o644)
}

func toggle(list []string, id string, on bool) []string {
	list = slices.DeleteFunc(list, func(x string) bool { return x == id })
	if on {
		list = append(list, id)
	}
	return list
}

func ApplyPassthrough(ctx context.Context) error {
	var errs []error
	p, err := readPassthrough()
	if err != nil {
		return err
	}
	for _, addr := range p.PCI {
		errs = append(errs, bindVFIO(ctx, addr, true))
	}
	return errors.Join(errs...)
}
