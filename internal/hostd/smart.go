package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type Partition struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	FSType string `json:"fstype"`
	Label  string `json:"label"`
}

type SelfTest struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Hours  int    `json:"hours"`
}

type Attribute struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Value  int    `json:"value"`
	Worst  int    `json:"worst"`
	Thresh int    `json:"thresh"`
	Raw    string `json:"raw"`
}

type Smart struct {
	Supported  bool        `json:"supported"`
	Message    string      `json:"message"`
	Passed     bool        `json:"passed"`
	Temp       int         `json:"temp"`
	Hours      int         `json:"hours"`
	Cycles     int         `json:"cycles"`
	Wear       int         `json:"wear"`
	Running    string      `json:"running"`
	Tests      []SelfTest  `json:"tests"`
	Attributes []Attribute `json:"attributes"`
}

type DiskDetail struct {
	Disk       Disk        `json:"disk"`
	Partitions []Partition `json:"partitions"`
	Smart      Smart       `json:"smart"`
}

type smartctl struct {
	Messages []struct {
		String string `json:"string"`
	} `json:"messages"`
	SmartSupport struct {
		Available bool `json:"available"`
	} `json:"smart_support"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature struct {
		Current int `json:"current"`
	} `json:"temperature"`
	PowerOnTime struct {
		Hours int `json:"hours"`
	} `json:"power_on_time"`
	PowerCycleCount int `json:"power_cycle_count"`
	ATA             struct {
		SelfTest struct {
			Status struct {
				String           string `json:"string"`
				RemainingPercent *int   `json:"remaining_percent"`
			} `json:"status"`
		} `json:"self_test"`
	} `json:"ata_smart_data"`
	ATALog struct {
		Standard struct {
			Table []struct {
				Type struct {
					String string `json:"string"`
				} `json:"type"`
				Status struct {
					String string `json:"string"`
				} `json:"status"`
				LifetimeHours int `json:"lifetime_hours"`
			} `json:"table"`
		} `json:"standard"`
	} `json:"ata_smart_self_test_log"`
	ATAAttrs struct {
		Table []struct {
			ID     int    `json:"id"`
			Name   string `json:"name"`
			Value  int    `json:"value"`
			Worst  int    `json:"worst"`
			Thresh int    `json:"thresh"`
			Raw    struct {
				String string `json:"string"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	NVMe *struct {
		PercentageUsed int `json:"percentage_used"`
	} `json:"nvme_smart_health_information_log"`
	NVMeLog struct {
		Current *struct {
			Operation struct {
				String string `json:"string"`
			} `json:"operation"`
			Completion int `json:"completion"`
		} `json:"current_self_test_operation"`
		Table []struct {
			Code struct {
				String string `json:"string"`
			} `json:"self_test_code"`
			Result struct {
				String string `json:"string"`
			} `json:"self_test_result"`
			Hours int `json:"power_on_hours"`
		} `json:"table"`
	} `json:"nvme_self_test_log"`
}

func readSmart(ctx context.Context, path string) Smart {
	var sm Smart
	if _, err := exec.LookPath("smartctl"); err != nil {
		sm.Message = "smartmontools is not installed"
		return sm
	}
	out, _ := exec.CommandContext(ctx, "smartctl", "-j", "-a", path).Output()
	var raw smartctl
	if err := json.Unmarshal(out, &raw); err != nil {
		sm.Message = "could not read SMART data"
		return sm
	}
	for _, m := range raw.Messages {
		sm.Message = m.String
	}
	sm.Supported = raw.SmartSupport.Available || raw.NVMe != nil
	if !sm.Supported {
		if sm.Message == "" {
			sm.Message = "this device does not report SMART data"
		}
		return sm
	}
	sm.Passed = raw.SmartStatus != nil && raw.SmartStatus.Passed
	sm.Temp, sm.Hours, sm.Cycles = raw.Temperature.Current, raw.PowerOnTime.Hours, raw.PowerCycleCount
	if raw.NVMe != nil {
		sm.Wear = raw.NVMe.PercentageUsed
	}
	if st := raw.ATA.SelfTest.Status; st.RemainingPercent != nil {
		sm.Running = fmt.Sprintf("%d%% remaining", *st.RemainingPercent)
	}
	if c := raw.NVMeLog.Current; c != nil && c.Operation.String != "" && !strings.HasPrefix(c.Operation.String, "No ") {
		sm.Running = fmt.Sprintf("%s, %d%% complete", c.Operation.String, c.Completion)
	}
	for _, t := range raw.ATALog.Standard.Table {
		sm.Tests = append(sm.Tests, SelfTest{Type: t.Type.String, Status: t.Status.String, Hours: t.LifetimeHours})
	}
	for _, t := range raw.NVMeLog.Table {
		sm.Tests = append(sm.Tests, SelfTest{Type: t.Code.String, Status: t.Result.String, Hours: t.Hours})
	}
	for _, a := range raw.ATAAttrs.Table {
		sm.Attributes = append(sm.Attributes, Attribute{ID: a.ID, Name: a.Name, Value: a.Value, Worst: a.Worst, Thresh: a.Thresh, Raw: a.Raw.String})
	}
	return sm
}

func findDisk(ctx context.Context, name string) (Disk, error) {
	disks, err := listDisks(ctx)
	if err != nil {
		return Disk{}, err
	}
	for _, d := range disks {
		if d.Name == name {
			return d, nil
		}
	}
	return Disk{}, fmt.Errorf("disk %s not found", name)
}

func diskDetail(ctx context.Context, name string) (DiskDetail, error) {
	d, err := findDisk(ctx, name)
	if err != nil {
		return DiskDetail{}, err
	}
	dd := DiskDetail{Disk: d}
	out, err := run(ctx, "lsblk", "-J", "-b", "-o", "NAME,PATH,TYPE,FSTYPE,LABEL,PTTYPE,TRAN,MODEL,SERIAL,SIZE,ROTA,MOUNTPOINTS", d.Path)
	if err == nil {
		var tree struct {
			Blockdevices []lsblkDev `json:"blockdevices"`
		}
		if err := json.Unmarshal([]byte(out), &tree); err != nil {
			return dd, fmt.Errorf("lsblk %s: %w", d.Path, err)
		}
		for _, b := range tree.Blockdevices {
			for _, c := range b.Children {
				dd.Partitions = append(dd.Partitions, Partition{Name: c.Name, Size: c.Size, FSType: c.Fstype, Label: c.Label})
			}
		}
	}
	dd.Smart = readSmart(ctx, d.Path)
	return dd, nil
}

func wipeDisk(ctx context.Context, name, confirm string) error {
	d, err := findDisk(ctx, name)
	if err != nil {
		return err
	}
	switch {
	case !d.Free():
		return fmt.Errorf("%s is in use (%s)", name, d.Use)
	case confirm != name:
		return fmt.Errorf("type %s to confirm erasing it", name)
	}
	return wipe(ctx, d.Path)
}

func smartTest(ctx context.Context, name, kind string) error {
	if kind != "short" && kind != "long" {
		return errors.New("test must be short or long")
	}
	d, err := findDisk(ctx, name)
	if err != nil {
		return err
	}
	if !readSmart(ctx, d.Path).Supported {
		return fmt.Errorf("%s does not support SMART", name)
	}
	_, err = run(ctx, "smartctl", "-t", kind, d.Path)
	return err
}
