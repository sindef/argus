package hostd

import (
	"bufio"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	drrBegin         = 0
	drrObject        = 1
	drrWrite         = 3
	drrFree          = 4
	drrEnd           = 5
	drrSpill         = 7
	drrWriteEmbedded = 8
	drrHeader        = 312
	zvolObject       = 1
	backupMagic      = 0x2F5BACBAC
)

type Extent [2]int64

type Changes struct {
	Size int64    `json:"size"`
	Data []Extent `json:"data"`
	Zero []Extent `json:"zero"`
}

func addExtent(xs []Extent, off, n int64) []Extent {
	if n <= 0 {
		return xs
	}
	return append(xs, Extent{off, n})
}

func merged(xs []Extent) []Extent {
	slices.SortFunc(xs, func(a, b Extent) int { return cmp.Compare(a[0], b[0]) })
	out := []Extent{}
	for _, x := range xs {
		if l := len(out) - 1; l >= 0 && out[l][0]+out[l][1] >= x[0] {
			out[l][1] = max(out[l][1], x[0]+x[1]-out[l][0])
			continue
		}
		out = append(out, x)
	}
	return out
}

func parseStream(r io.Reader, size int64) (Changes, error) {
	ch := Changes{Size: size, Data: []Extent{}, Zero: []Extent{}}
	h := make([]byte, drrHeader)
	var bo binary.ByteOrder = binary.LittleEndian
	for first := true; ; first = false {
		if _, err := io.ReadFull(r, h); err != nil {
			return ch, fmt.Errorf("truncated send stream: %w", err)
		}
		if first {
			switch {
			case binary.LittleEndian.Uint64(h[8:]) == backupMagic:
			case binary.BigEndian.Uint64(h[8:]) == backupMagic:
				bo = binary.BigEndian
			default:
				return ch, errors.New("not a zfs send stream")
			}
		}
		u64 := func(o int) int64 { return int64(bo.Uint64(h[o:])) }
		u32 := func(o int) int64 { return int64(bo.Uint32(h[o:])) }
		payload := u32(4)
		switch bo.Uint32(h[0:]) {
		case drrEnd:
			ch.Data, ch.Zero = merged(ch.Data), merged(ch.Zero)
			return ch, nil
		case drrObject:
			payload = (u32(28) + 7) &^ 7
		case drrWrite:
			if payload = u64(32); h[50] != 0 {
				payload = u64(96)
			}
			if u64(8) == zvolObject {
				ch.Data = addExtent(ch.Data, u64(24), u64(32))
			}
		case drrFree:
			if u64(8) == zvolObject && u64(16) < size {
				n := u64(24)
				if n < 0 || u64(16)+n > size {
					n = size - u64(16)
				}
				ch.Zero = addExtent(ch.Zero, u64(16), n)
			}
		case drrWriteEmbedded:
			payload = (u32(52) + 7) &^ 7
			if u64(8) == zvolObject {
				ch.Data = addExtent(ch.Data, u64(16), u64(24))
			}
		case drrSpill:
			payload = u64(16)
		}
		if _, err := io.CopyN(io.Discard, r, payload); err != nil {
			return ch, fmt.Errorf("truncated send stream: %w", err)
		}
	}
}

func volsize(ctx context.Context, ds string) (int64, error) {
	out, err := run(ctx, "zfs", "get", "-Hp", "-o", "value", "volsize", ds)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

func zfsChanges(ctx context.Context, ds, snap, from string) (Changes, error) {
	if !validDataset.MatchString(ds) || !validZSnap.MatchString(snap) || (from != "" && !validZSnap.MatchString(from)) {
		return Changes{}, errors.New("invalid changes request")
	}
	size, err := volsize(ctx, ds+"@"+snap)
	if err != nil {
		return Changes{}, err
	}
	args := []string{"send", "-c"}
	if from != "" {
		args = append(args, "-i", "@"+from)
	}
	cmd := exec.CommandContext(ctx, "zfs", append(args, ds+"@"+snap)...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return Changes{}, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Changes{}, err
	}
	ch, perr := parseStream(bufio.NewReaderSize(out, 1<<20), size)
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); err != nil {
		return Changes{}, fmt.Errorf("zfs send %s@%s: %w: %s", ds, snap, err, strings.TrimSpace(stderr.String()))
	}
	return ch, perr
}

func zfsRead(w http.ResponseWriter, r *http.Request) {
	ds, snap := r.URL.Query().Get("ds"), r.URL.Query().Get("snap")
	if !validDataset.MatchString(ds) || !validZSnap.MatchString(snap) || !strings.HasPrefix(snap, "argus-cbt-") {
		reply(w, nil, errors.New("invalid read request"))
		return
	}
	if _, err := run(r.Context(), "zfs", "set", "snapdev=visible", ds); err != nil {
		reply(w, nil, err)
		return
	}
	dev := "/dev/zvol/" + ds + "@" + snap
	var f *os.File
	var err error
	for deadline := time.Now().Add(15 * time.Second); ; {
		if f, err = os.Open(dev); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		reply(w, nil, err)
		return
	}
	defer f.Close()
	size, err := volsize(r.Context(), ds+"@"+snap)
	if err != nil {
		reply(w, nil, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, io.NewSectionReader(f, 0, size))
}

func zfsDestroy(ctx context.Context, datasets []string, snap string) error {
	if !validZSnap.MatchString(snap) || !strings.HasPrefix(snap, "argus-cbt-") {
		return errors.New("only backup snapshots can be destroyed")
	}
	for _, ds := range datasets {
		if _, err := run(ctx, "zfs", "destroy", ds+"@"+snap); err != nil && !strings.Contains(err.Error(), "could not find any snapshots") && !strings.Contains(err.Error(), "does not exist") {
			return err
		}
	}
	return nil
}
