package hostd

import (
	"os"
	"slices"
	"testing"
)

func covers(xs []Extent, off, n int64) bool {
	return slices.ContainsFunc(xs, func(x Extent) bool { return x[0] <= off && off+n <= x[0]+x[1] })
}

func TestParseStream(t *testing.T) {
	const size = 16 << 20
	for _, c := range []struct {
		file string
		data []Extent
		zero []Extent
	}{
		{"full.zs", []Extent{{0, 8192}, {40960, 4096}, {409600, 16384}}, []Extent{{8192, 32768}, {425984, size - 425984}}},
		{"incr.zs", []Extent{{40960, 4096}, {819200, 8192}}, []Extent{{409600, 16384}}},
	} {
		f, err := os.Open("testdata/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		ch, err := parseStream(f, size)
		f.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if !slices.Equal(ch.Data, c.data) {
			t.Errorf("%s: data %v, want %v", c.file, ch.Data, c.data)
		}
		for _, z := range c.zero {
			if !covers(ch.Zero, z[0], z[1]) {
				t.Errorf("%s: zero %v not covered by %v", c.file, z, ch.Zero)
			}
		}
		for _, d := range ch.Data {
			for _, z := range ch.Zero {
				if d[0] < z[0]+z[1] && z[0] < d[0]+d[1] {
					t.Errorf("%s: data %v overlaps zero %v", c.file, d, z)
				}
			}
		}
	}
}
