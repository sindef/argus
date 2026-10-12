package hostd

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var csiSnap = regexp.MustCompile(`^(snapshot|pvc)-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type zsnap struct {
	name    string
	clones  []string
	created time.Time
}

func listZSnaps(ctx context.Context, args ...string) ([]zsnap, error) {
	out, err := run(ctx, "zfs", append([]string{"list", "-H", "-p", "-t", "snapshot", "-o", "name,clones,creation", "-S", "creation"}, args...)...)
	if err != nil {
		return nil, err
	}
	var snaps []zsnap
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		s := zsnap{name: f[0]}
		if f[1] != "-" && f[1] != "" {
			s.clones = strings.Split(f[1], ",")
		}
		if sec, err := strconv.ParseInt(f[2], 10, 64); err == nil {
			s.created = time.Unix(sec, 0)
		}
		snaps = append(snaps, s)
	}
	return snaps, nil
}

func ReleaseVolume(ctx context.Context, dataset string) error {
	if !validDataset.MatchString(dataset) {
		return errors.New("invalid dataset")
	}
	for range 16 {
		snaps, err := listZSnaps(ctx, "-d", "1", dataset)
		if err != nil {
			return err
		}
		var promote string
		for _, s := range snaps {
			switch {
			case len(s.clones) == 0:
				if _, err := run(ctx, "zfs", "destroy", s.name); err != nil {
					return err
				}
			case promote == "":
				promote = s.clones[0]
			}
		}
		if promote == "" {
			return nil
		}
		if _, err := run(ctx, "zfs", "promote", promote); err != nil {
			return fmt.Errorf("promote %s: %w", promote, err)
		}
	}
	return fmt.Errorf("%s still has dependent clones", dataset)
}

func SweepSnapshots(ctx context.Context, keep []string) ([]string, error) {
	snaps, err := listZSnaps(ctx)
	if err != nil {
		return nil, err
	}
	var gone []string
	for _, s := range snaps {
		ds, snap, _ := strings.Cut(s.name, "@")
		if !validDataset.MatchString(ds) || !csiSnap.MatchString(snap) || slices.Contains(keep, snap) || len(s.clones) > 0 || time.Since(s.created) < 10*time.Minute {
			continue
		}
		if _, err := run(ctx, "zfs", "destroy", s.name); err != nil {
			return gone, err
		}
		gone = append(gone, s.name)
	}
	return gone, nil
}
