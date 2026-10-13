package hostui

import (
	"errors"
	"net/http"
	"slices"
	"strconv"

	"argus/internal/kube"
	"argus/internal/ui"
	"argus/internal/wire"
)

func ParseNetwork(r *http.Request) (wire.NetworkRequest, error) {
	n := wire.NetworkRequest{Name: r.FormValue("name"), Bridge: r.FormValue("bridge"), SpoofCheck: r.FormValue("spoofcheck") != ""}
	var err error
	if v := r.FormValue("vlan"); v != "" {
		n.VLAN, err = strconv.Atoi(v)
	}
	if v := r.FormValue("rate"); v != "" && err == nil {
		n.RateMbps, err = strconv.Atoi(v)
	}
	return n, err
}

func ValidNetwork(n wire.NetworkRequest) error {
	if n.VLAN < 0 || n.VLAN > 4094 {
		return errors.New("VLAN must be 1-4094")
	}
	if !ui.ValidObject.MatchString(n.Name) {
		return errors.New("invalid name")
	}
	if n.RateMbps < 0 || n.RateMbps > 100000 {
		return errors.New("bandwidth limit must be 0-100000 Mbit/s")
	}
	return nil
}

func FormVolume(r *http.Request) wire.VolumeRequest {
	var size int
	_ = ui.IntField(r, "size", 1, 4096, &size)
	return wire.VolumeRequest{Name: r.FormValue("name"), Size: size, URL: r.FormValue("url"), VM: r.FormValue("vm"), Pool: r.FormValue("pool")}
}

func VolumePools(vols []HostVolume) []string {
	var out []string
	for _, v := range vols {
		if p := v.Pool(); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	if len(out) < 2 {
		return nil
	}
	return out
}

func StoppedVMs(vms []kube.VM) []string {
	var out []string
	for _, v := range vms {
		if !v.Running {
			out = append(out, v.Name)
		}
	}
	return out
}
