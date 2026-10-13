package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	flrRun  = "/run/argus-hostd/flr"
	flrData = "/run/argus-hostd/flr-data"
)

var (
	validFLR     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	validFLRDisk = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,62})$`)
	validFLRPart = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

type FLRPart struct {
	Name string `json:"name"`
	FS   string `json:"fs"`
	Dev  string `json:"dev"`
	Opts string `json:"opts"`
}

type FLREntry struct {
	Name     string    `json:"name"`
	Dir      bool      `json:"dir"`
	Link     bool      `json:"link"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func flrDataset(ctx context.Context) error {
	out, err := run(ctx, "zpool", "list", "-H", "-p", "-o", "name,free")
	if err != nil {
		return err
	}
	pool, best := "", int64(-1)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			if free, _ := strconv.ParseInt(f[1], 10, 64); free > best {
				pool, best = f[0], free
			}
		}
	}
	if pool == "" {
		return errors.New("no storage pool for unpacking the backup")
	}
	if mounted(flrData) {
		return nil
	}
	if _, err := run(ctx, "zfs", "list", "-H", "-o", "name", pool+"/argus-flr"); err != nil {
		_, err = run(ctx, "zfs", "create", "-o", "mountpoint="+flrData, "-o", "compression=lz4", pool+"/argus-flr")
		return err
	}
	_, err = run(ctx, "zfs", "mount", pool+"/argus-flr")
	return err
}

func sparseCopy(f *os.File, r io.Reader) error {
	buf := make([]byte, 1<<20)
	var size int64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if allZeroBytes(buf[:n]) {
				if _, err := f.Seek(int64(n), io.SeekCurrent); err != nil {
					return err
				}
			} else if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			size += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return f.Truncate(size)
		}
		if err != nil {
			return err
		}
	}
}

func allZeroBytes(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func FLRLoad(ctx context.Context, id, disk string, r io.Reader) error {
	if !validFLR.MatchString(id) || !validFLRDisk.MatchString(disk) {
		return errors.New("invalid session")
	}
	if err := flrDataset(ctx); err != nil {
		return err
	}
	dir := filepath.Join(flrRun, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	img := filepath.Join(flrData, id+"-"+disk+".img")
	f, err := os.OpenFile(img, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = sparseCopy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(img)
		return err
	}
	return flrAttach(ctx, id, disk, img)
}

func FLRAttachVolume(ctx context.Context, id, disk, dataset string) error {
	if !validFLR.MatchString(id) || !validFLRDisk.MatchString(disk) || !validDataset.MatchString(dataset) {
		return errors.New("invalid session")
	}
	if err := os.MkdirAll(filepath.Join(flrRun, id), 0o700); err != nil {
		return err
	}
	return flrAttach(ctx, id, disk, "/dev/zvol/"+dataset)
}

func flrAttach(ctx context.Context, id, disk, source string) error {
	dir := filepath.Join(flrRun, id)
	dev, err := run(ctx, "losetup", "--find", "--show", "--read-only", "--partscan", source)
	if err != nil {
		return err
	}
	dev = strings.TrimSpace(dev)
	if err := os.WriteFile(filepath.Join(dir, disk+".loop"), []byte(dev), 0o600); err != nil {
		return err
	}
	time.Sleep(time.Second)
	parts, _ := filepath.Glob(dev + "p*")
	if len(parts) == 0 {
		parts = []string{dev}
	}
	var mountedParts []FLRPart
	for _, p := range parts {
		fsType, _ := run(ctx, "blkid", "-o", "value", "-s", "TYPE", p)
		fsType = strings.TrimSpace(fsType)
		opts := "ro,nosuid,nodev,noexec"
		switch fsType {
		case "ext2", "ext3", "ext4":
			opts += ",noload"
		case "xfs":
			opts += ",norecovery,nouuid"
		case "ntfs":
			fsType = "ntfs3"
		case "vfat", "exfat", "btrfs":
		default:
			continue
		}
		name := disk + "-" + strings.TrimPrefix(p, dev)
		if p == dev {
			name = disk
		}
		part := FLRPart{Name: name, FS: fsType, Dev: p, Opts: opts}
		if withMount(ctx, id, part, func(*os.Root) error { return nil }) == nil {
			mountedParts = append(mountedParts, part)
		}
	}
	all, _ := FLRParts(id)
	b, _ := json.Marshal(append(all, mountedParts...))
	return os.WriteFile(filepath.Join(dir, "parts.json"), b, 0o600)
}

func FLRParts(id string) ([]FLRPart, error) {
	if !validFLR.MatchString(id) {
		return nil, errors.New("invalid session")
	}
	var parts []FLRPart
	b, err := os.ReadFile(filepath.Join(flrRun, id, "parts.json"))
	if err == nil {
		err = json.Unmarshal(b, &parts)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return parts, err
}

func withMount(ctx context.Context, id string, part FLRPart, fn func(*os.Root) error) error {
	mp, err := os.MkdirTemp(filepath.Join(flrRun, id), "m-")
	if err != nil {
		return err
	}
	defer os.Remove(mp)
	if _, err := run(ctx, "mount", "-t", part.FS, "-o", part.Opts, part.Dev, mp); err != nil {
		return err
	}
	defer func() {
		if _, err := run(context.Background(), "umount", mp); err != nil {
			log.Printf("file browse: %v", err)
		}
	}()
	root, err := os.OpenRoot(mp)
	if err != nil {
		return err
	}
	defer root.Close()
	return fn(root)
}

func flrPart(ctx context.Context, id, name string, fn func(*os.Root) error) error {
	parts, err := FLRParts(id)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(parts, func(p FLRPart) bool { return p.Name == name })
	if !validFLRPart.MatchString(name) || i < 0 {
		return errors.New("partition not found")
	}
	return withMount(ctx, id, parts[i], fn)
}

func cleanRel(p string) string {
	p = strings.Trim(filepath.ToSlash(filepath.Clean("/"+p)), "/")
	if p == "" {
		return "."
	}
	return p
}

func FLRList(ctx context.Context, id, part, path string) ([]FLREntry, error) {
	var out []FLREntry
	err := flrPart(ctx, id, part, func(root *os.Root) error {
		d, err := root.Open(cleanRel(path))
		if err != nil {
			return err
		}
		defer d.Close()
		des, err := d.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, de := range des {
			e := FLREntry{Name: de.Name(), Dir: de.IsDir(), Link: de.Type()&fs.ModeSymlink != 0}
			if info, err := de.Info(); err == nil {
				e.Size, e.Modified = info.Size(), info.ModTime()
			}
			out = append(out, e)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, err
}

func FLRCopy(ctx context.Context, id, part, path string, start func(size int64) io.Writer) error {
	return flrPart(ctx, id, part, func(root *os.Root) error {
		f, err := root.Open(cleanRel(path))
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("not a file")
		}
		_, err = io.Copy(start(info.Size()), f)
		return err
	})
}

func FLRClose(ctx context.Context, id string) error {
	if !validFLR.MatchString(id) {
		return errors.New("invalid session")
	}
	dir := filepath.Join(flrRun, id)
	loops, _ := filepath.Glob(filepath.Join(dir, "*.loop"))
	for _, l := range loops {
		if dev, err := os.ReadFile(l); err == nil {
			if _, err := run(ctx, "losetup", "-d", strings.TrimSpace(string(dev))); err != nil {
				log.Printf("file browse: %v", err)
			}
		}
	}
	imgs, _ := filepath.Glob(filepath.Join(flrData, id+"-*.img"))
	for _, img := range imgs {
		os.Remove(img)
	}
	return os.RemoveAll(dir)
}

func FLRSweep(ctx context.Context, maxAge time.Duration) []string {
	var closed []string
	sessions, _ := os.ReadDir(flrRun)
	for _, s := range sessions {
		info, err := s.Info()
		if err == nil && validFLR.MatchString(s.Name()) && time.Since(info.ModTime()) > maxAge {
			if FLRClose(ctx, s.Name()) == nil {
				closed = append(closed, s.Name())
			}
		}
	}
	imgs, _ := filepath.Glob(filepath.Join(flrData, "*.img"))
	for _, img := range imgs {
		id, _, _ := strings.Cut(filepath.Base(img), "-")
		if _, err := os.Stat(filepath.Join(flrRun, id)); errors.Is(err, fs.ErrNotExist) {
			os.Remove(img)
			closed = append(closed, fmt.Sprintf("orphan %s", filepath.Base(img)))
		}
	}
	return closed
}
