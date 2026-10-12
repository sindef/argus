package hostd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

var (
	validDataset = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*/pvc-[0-9a-f-]{36}$`)
	validZSnap   = regexp.MustCompile(`^argus-zrepl-[0-9]+$`)
)

func checkDatasets(ds []string) error {
	for _, d := range ds {
		if !validDataset.MatchString(d) {
			return fmt.Errorf("invalid dataset %q", d)
		}
	}
	return nil
}

func zfsSnapshots(ctx context.Context, ds string) ([]string, error) {
	out, err := run(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-s", "createtxg", "-d", "1", ds)
	if err != nil {
		return nil, err
	}
	var snaps []string
	for _, l := range strings.Fields(out) {
		if _, s, ok := strings.Cut(l, "@"); ok && validZSnap.MatchString(s) {
			snaps = append(snaps, s)
		}
	}
	return snaps, nil
}

func zfsPrune(ctx context.Context, datasets []string, keep []string) error {
	for _, ds := range datasets {
		snaps, err := zfsSnapshots(ctx, ds)
		if err != nil {
			return err
		}
		for _, s := range snaps {
			if !slices.Contains(keep, s) {
				if _, err := run(ctx, "zfs", "destroy", ds+"@"+s); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func zfsSnapshot(ctx context.Context, datasets []string, snap string) error {
	args := []string{"snapshot"}
	for _, ds := range datasets {
		args = append(args, ds+"@"+snap)
	}
	_, err := run(ctx, "zfs", args...)
	return err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func zfsSend(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ds, snap, from := q.Get("ds"), q.Get("snap"), q.Get("from")
	if !validDataset.MatchString(ds) || !validZSnap.MatchString(snap) || (from != "" && !validZSnap.MatchString(from)) {
		reply(w, nil, fmt.Errorf("invalid send request"))
		return
	}
	args := []string{"send", "-c"}
	if from != "" {
		args = append(args, "-i", "@"+from)
	}
	cmd := exec.CommandContext(r.Context(), "zfs", append(args, ds+"@"+snap)...)
	var stderr bytes.Buffer
	cw := &countingWriter{w: w}
	cmd.Stdout, cmd.Stderr = cw, &stderr
	w.Header().Set("Content-Type", "application/octet-stream")
	if err := cmd.Run(); err != nil {
		err = fmt.Errorf("zfs send %s@%s: %v: %s", ds, snap, err, strings.TrimSpace(stderr.String()))
		if cw.n == 0 {
			w.Header().Del("Content-Type")
			reply(w, nil, err)
			return
		}
		log.Printf("%v", err)
	}
}

func zfsRecv(w http.ResponseWriter, r *http.Request) {
	ds, full := r.URL.Query().Get("ds"), r.URL.Query().Get("full") == "1"
	if !validDataset.MatchString(ds) {
		reply(w, nil, fmt.Errorf("invalid dataset %q", ds))
		return
	}
	if full {
		if err := zfsPrune(r.Context(), []string{ds}, nil); err != nil {
			reply(w, nil, err)
			return
		}
	}
	cmd := exec.CommandContext(r.Context(), "zfs", "recv", "-F", ds)
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stderr = r.Body, &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("zfs recv %s: %v: %s", ds, err, strings.TrimSpace(stderr.String()))
	}
	log.Printf("zfs recv %s full=%v: %v", ds, full, err)
	reply(w, nil, err)
}
