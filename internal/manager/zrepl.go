package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"

	"argus/internal/kube"
	"argus/internal/lifecycle"
	"argus/internal/wire"
)

var (
	errNoZFS   = errors.New("not on ZFS")
	errPartial = errors.New("replica disks are from different points in time until the next successful sync")
)

func zbase(owner, replica kube.ZDisk) string {
	for i := len(replica.Snapshots) - 1; i >= 0; i-- {
		if slices.Contains(owner.Snapshots, replica.Snapshots[i]) {
			return replica.Snapshots[i]
		}
	}
	return ""
}

func zsync(ctx context.Context, vm string, owner, replica Node) error {
	var od, rd map[string]kube.ZDisk
	if owner.do(ctx, http.MethodGet, vmPath(vm, "zfs"), nil, &od) != nil || replica.do(ctx, http.MethodGet, vmPath(vm, "zfs"), nil, &rd) != nil || len(od) != len(rd) {
		return errNoZFS
	}
	bases, mapping := map[string]string{}, map[string]string{}
	var keep []string
	for v, d := range od {
		r, ok := rd[v]
		if !ok {
			return errNoZFS
		}
		bases[v] = zbase(d, r)
		mapping[d.DataVolume] = r.DataVolume
		if bases[v] != "" && !slices.Contains(keep, bases[v]) {
			keep = append(keep, bases[v])
		}
	}
	var snap wire.ZFSSnapshot
	if err := owner.do(ctx, http.MethodPost, vmPath(vm, "zfs/snapshot"), wire.ZFSSnapshotRequest{Keep: keep}, &snap); err != nil {
		return err
	}
	received := 0
	for v := range od {
		if err := zpipe(ctx, vm, v, snap.Snap, bases[v], owner, replica); err != nil {
			if received > 0 || bases[v] == "" {
				return fmt.Errorf("disk %s: %w; %w", v, err, errPartial)
			}
			return fmt.Errorf("disk %s: %w", v, err)
		}
		received++
	}
	if err := replica.do(ctx, http.MethodPost, vmPath(vm, "replica/swap"), wire.ReplicaRequest{Export: kube.Export{Spec: snap.Spec}, Mapping: mapping}, nil); err != nil {
		return err
	}
	done := map[string]any{"keep": []string{snap.Snap}}
	lifecycle.LogCleanup("snapshots of "+vm+" on "+owner.Name, owner.do(ctx, http.MethodPost, vmPath(vm, "zfs/prune"), done, nil))
	return replica.do(ctx, http.MethodPost, vmPath(vm, "zfs/prune"), done, nil)
}

func zpipe(ctx context.Context, vm, volume, snap, base string, owner, replica Node) error {
	src, err := owner.Open(ctx, http.MethodGet, vmPath(vm, "zfs/send")+"?"+url.Values{"volume": {volume}, "snap": {snap}, "from": {base}}.Encode(), nil, nil)
	if err != nil {
		return err
	}
	defer src.Body.Close()
	q := url.Values{"volume": {volume}}
	if base == "" {
		q.Set("full", "1")
	}
	dst, err := replica.Open(ctx, http.MethodPut, vmPath(vm, "zfs/recv")+"?"+q.Encode(), http.Header{"Content-Type": {"application/octet-stream"}}, src.Body)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, dst.Body)
	return errors.Join(err, dst.Body.Close())
}
