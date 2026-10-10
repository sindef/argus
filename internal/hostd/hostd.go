package hostd

type Disk struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	ID         string   `json:"id"`
	Model      string   `json:"model"`
	Serial     string   `json:"serial"`
	Transport  string   `json:"transport"`
	Size       int64    `json:"size"`
	Media      string   `json:"media"`
	Use        string   `json:"use"`
	Pool       string   `json:"pool"`
	Signatures []string `json:"signatures"`
}

func (d Disk) Free() bool  { return d.Use == "" }
func (d Disk) Blank() bool { return len(d.Signatures) == 0 }

type Vdev struct {
	Type  string   `json:"type"`
	Disks []string `json:"disks"`
}

type PoolSpec struct {
	Name    string   `json:"name"`
	Data    []Vdev   `json:"data"`
	Log     []Vdev   `json:"log"`
	Special []Vdev   `json:"special"`
	Cache   []string `json:"cache"`
	Spares  []string `json:"spares"`
	Wipe    []string `json:"wipe"`
}

type Pool struct {
	Name   string `json:"name"`
	Health string `json:"health"`
	Size   int64  `json:"size"`
	Alloc  int64  `json:"alloc"`
	Free   int64  `json:"free"`
	Status string `json:"status"`
	System bool   `json:"system"`
	Media  string `json:"media"`
}

type Importable struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	State string `json:"state"`
}

type Pools struct {
	Pools      []Pool       `json:"pools"`
	Importable []Importable `json:"importable"`
}
