package hostd

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

type ZVol struct {
	Size      int64 `json:"size"`
	Allocated int64 `json:"allocated"`
	SnapUsed  int64 `json:"snapUsed"`
	Thick     bool  `json:"thick"`
}

func ZVols(ctx context.Context, datasets []string) (map[string]ZVol, error) {
	out := map[string]ZVol{}
	for _, ds := range datasets {
		if !validDataset.MatchString(ds) {
			return nil, errors.New("invalid dataset")
		}
	}
	if len(datasets) == 0 {
		return out, nil
	}
	res, err := run(ctx, "zfs", append([]string{"get", "-H", "-p", "-o", "name,property,value", "volsize,referenced,usedbysnapshots,refreservation"}, datasets...)...)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(res), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			continue
		}
		n, _ := strconv.ParseInt(f[2], 10, 64)
		z := out[f[0]]
		switch f[1] {
		case "volsize":
			z.Size = n
		case "referenced":
			z.Allocated = n
		case "usedbysnapshots":
			z.SnapUsed = n
		case "refreservation":
			z.Thick = n > 0
		}
		out[f[0]] = z
	}
	return out, nil
}

func SetThick(ctx context.Context, dataset string, thick bool) error {
	if !validDataset.MatchString(dataset) {
		return errors.New("invalid dataset")
	}
	v := "none"
	if thick {
		v = "auto"
	}
	_, err := run(ctx, "zfs", "set", "refreservation="+v, dataset)
	return err
}
