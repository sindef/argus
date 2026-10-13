package hostd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const poolsFile = "/usr/local/argus/pools"

var (
	validPool    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	reservedPool = []string{"zfs", "mirror", "raidz", "raidz1", "raidz2", "raidz3", "spare", "log", "cache", "special", "draid"}
	minDisks     = map[string]int{"stripe": 1, "mirror": 2, "raidz1": 3, "raidz2": 4, "raidz3": 5}
)

func systemPool() string {
	f, err := os.Open("/etc/argus/datastore.env")
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "ARGUS_POOL="); ok && v != "" {
				return v
			}
		}
	}
	return "argus"
}

func listPools(ctx context.Context) (Pools, error) {
	var res Pools
	out, err := run(ctx, "zpool", "list", "-Hp", "-o", "name,size,alloc,free,health")
	if err != nil {
		return res, err
	}
	disks, _ := listDisks(ctx)
	sys := systemPool()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 5 {
			continue
		}
		p := Pool{Name: f[0], Health: f[4], System: f[0] == sys}
		p.Size, _ = strconv.ParseInt(f[1], 10, 64)
		p.Alloc, _ = strconv.ParseInt(f[2], 10, 64)
		p.Free, _ = strconv.ParseInt(f[3], 10, 64)
		p.Status, _ = run(ctx, "zpool", "status", p.Name)
		for _, d := range disks {
			if d.Pool == p.Name && (p.Media == "" || d.Media == "hdd") {
				p.Media = d.Media
			}
		}
		res.Pools = append(res.Pools, p)
	}
	imp, _ := run(ctx, "zpool", "import")
	var cur *Importable
	for _, line := range strings.Split(imp, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "pool:":
			res.Importable = append(res.Importable, Importable{Name: f[1]})
			cur = &res.Importable[len(res.Importable)-1]
		case "id:":
			if cur != nil {
				cur.ID = f[1]
			}
		case "state:":
			if cur != nil {
				cur.State = f[1]
			}
		}
	}
	return res, nil
}

func vdevArgs(role string, vdevs []Vdev, ids map[string]string) ([]string, error) {
	var args []string
	for _, v := range vdevs {
		min, ok := minDisks[v.Type]
		if !ok || (role != "data" && v.Type != "stripe" && v.Type != "mirror") {
			return nil, fmt.Errorf("%s vdevs cannot be %q", role, v.Type)
		}
		if len(v.Disks) < min {
			return nil, fmt.Errorf("%s needs at least %d disks", v.Type, min)
		}
		if role != "data" {
			args = append(args, role)
		}
		if v.Type != "stripe" {
			args = append(args, v.Type)
		}
		for _, d := range v.Disks {
			args = append(args, ids[d])
		}
	}
	return args, nil
}

func (s *Server) prepare(ctx context.Context, spec PoolSpec) ([]string, error) {
	disks, err := listDisks(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]Disk{}
	for _, d := range disks {
		byName[d.Name] = d
	}
	var used []string
	collect := func(names []string) {
		used = append(used, names...)
	}
	for _, group := range [][]Vdev{spec.Data, spec.Log, spec.Special} {
		for _, v := range group {
			collect(v.Disks)
		}
	}
	collect(spec.Cache)
	collect(spec.Spares)
	if len(used) == 0 {
		return nil, errors.New("no disks selected")
	}
	ids := map[string]string{}
	seen := map[string]bool{}
	for _, n := range used {
		d, ok := byName[n]
		switch {
		case !ok:
			return nil, fmt.Errorf("disk %s not found", n)
		case seen[n]:
			return nil, fmt.Errorf("disk %s is used more than once", n)
		case !d.Free():
			return nil, fmt.Errorf("disk %s is in use (%s)", n, d.Use)
		case !d.Blank() && !slices.Contains(spec.Wipe, n):
			return nil, fmt.Errorf("disk %s has data (%s); confirm wiping it", n, strings.Join(d.Signatures, ", "))
		}
		seen[n], ids[n] = true, d.ID
	}
	for _, n := range spec.Wipe {
		if !seen[n] {
			return nil, fmt.Errorf("disk %s is marked for wiping but not selected", n)
		}
	}
	var args []string
	for _, part := range []struct {
		role  string
		vdevs []Vdev
	}{{"data", spec.Data}, {"log", spec.Log}, {"special", spec.Special}} {
		a, err := vdevArgs(part.role, part.vdevs, ids)
		if err != nil {
			return nil, err
		}
		args = append(args, a...)
	}
	for role, names := range map[string][]string{"cache": spec.Cache, "spare": spec.Spares} {
		if len(names) > 0 {
			args = append(args, role)
			for _, n := range names {
				args = append(args, ids[n])
			}
		}
	}
	for _, n := range spec.Wipe {
		if err := wipe(ctx, byName[n].Path); err != nil {
			return nil, err
		}
	}
	return args, nil
}

func wipe(ctx context.Context, disk string) error {
	out, _ := run(ctx, "lsblk", "-ln", "-o", "PATH", disk)
	parts := strings.Fields(out)
	for i := len(parts) - 1; i >= 0; i-- {
		_, _ = run(ctx, "zpool", "labelclear", "-f", parts[i])
		if _, err := run(ctx, "wipefs", "-a", parts[i]); err != nil {
			return err
		}
	}
	return nil
}

func persist(name string, add bool) error {
	b, _ := os.ReadFile(poolsFile)
	names := slices.DeleteFunc(strings.Fields(string(b)), func(n string) bool { return n == name })
	if add {
		names = append(names, name)
	}
	if err := os.MkdirAll("/usr/local/argus", 0o755); err != nil {
		return err
	}
	return os.WriteFile(poolsFile, []byte(strings.Join(names, "\n")+"\n"), 0o644)
}

func (s *Server) createPool(ctx context.Context, spec PoolSpec) error {
	if !validPool.MatchString(spec.Name) || slices.Contains(reservedPool, spec.Name) {
		return errors.New("pool name must be lowercase letters, digits and dashes, starting with a letter")
	}
	if _, err := run(ctx, "zpool", "list", "-H", spec.Name); err == nil {
		return fmt.Errorf("pool %s already exists", spec.Name)
	}
	if len(spec.Data) == 0 {
		return errors.New("a pool needs at least one data vdev")
	}
	args, err := s.prepare(ctx, spec)
	if err != nil {
		return err
	}
	create := append([]string{"create", "-f", "-o", "ashift=12", "-O", "compression=lz4", "-O", "atime=off", "-m", "none", spec.Name}, args...)
	if _, err := run(ctx, "zpool", create...); err != nil {
		return err
	}
	return persist(spec.Name, true)
}

func (s *Server) addToPool(ctx context.Context, name string, spec PoolSpec) error {
	if _, err := run(ctx, "zpool", "list", "-H", name); err != nil {
		return fmt.Errorf("pool %s not found", name)
	}
	args, err := s.prepare(ctx, spec)
	if err != nil {
		return err
	}
	_, err = run(ctx, "zpool", append([]string{"add", "-f", name}, args...)...)
	return err
}

func importPool(ctx context.Context, name string, force bool) error {
	if !validPool.MatchString(name) {
		return errors.New("invalid pool name")
	}
	args := []string{"import", "-N", name}
	if force {
		args = []string{"import", "-f", "-N", name}
	}
	if _, err := run(ctx, "zpool", args...); err != nil {
		if strings.Contains(err.Error(), "in use from another system") {
			return fmt.Errorf("pool %s was last used by another host; use Force import only if no other host is using these disks", name)
		}
		return err
	}
	return persist(name, true)
}

func releasePool(ctx context.Context, name, action, confirm string) error {
	if name == systemPool() {
		return errors.New("the system pool holds Argus itself and cannot be removed")
	}
	if action == "destroy" && confirm != name {
		return errors.New("type the pool name to destroy it")
	}
	out, err := run(ctx, "zfs", "list", "-H", "-o", "name", "-r", name)
	if err != nil {
		return fmt.Errorf("pool %s not found", name)
	}
	if n := len(strings.Fields(out)); n > 1 {
		return fmt.Errorf("pool %s still holds %d volumes; delete or move them first", name, n-1)
	}
	args := []string{"export", name}
	if action == "destroy" {
		args = []string{"destroy", "-f", name}
	}
	if _, err := run(ctx, "zpool", args...); err != nil {
		return err
	}
	return persist(name, false)
}
