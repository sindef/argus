package kube

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type VMCounters [6]float64

type MetricsSample struct {
	Time     time.Time             `json:"time"`
	CPUTotal float64               `json:"cpuTotal"`
	CPUIdle  float64               `json:"cpuIdle"`
	MemUsed  int64                 `json:"memUsed"`
	MemTotal int64                 `json:"memTotal"`
	PoolUsed int64                 `json:"poolUsed"`
	PoolFree int64                 `json:"poolFree"`
	VMs      map[string]VMCounters `json:"vms"`
}

var vmiMetrics = map[string]struct {
	i    int
	sign float64
}{
	"kubevirt_vmi_cpu_usage_seconds_total":           {0, 1},
	"kubevirt_vmi_memory_available_bytes":            {1, 1},
	"kubevirt_vmi_memory_usable_bytes":               {1, -1},
	"kubevirt_vmi_storage_read_traffic_bytes_total":  {2, 1},
	"kubevirt_vmi_storage_write_traffic_bytes_total": {3, 1},
	"kubevirt_vmi_network_receive_bytes_total":       {4, 1},
	"kubevirt_vmi_network_transmit_bytes_total":      {5, 1},
}

var labelRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

var metricsClient = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

func (c *Client) VMMetrics(ctx context.Context) (map[string]VMCounters, error) {
	pods, err := c.dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).Namespace("kubevirt").List(ctx, metav1.ListOptions{LabelSelector: "kubevirt.io=virt-handler"})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, errors.New("virt-handler not running")
	}
	ip, _, _ := unstructured.NestedString(pods.Items[0].Object, "status", "podIP")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://%s:8443/metrics", ip), nil)
	if err != nil {
		return nil, err
	}
	resp, err := metricsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := map[string]VMCounters{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		name, rest, ok := strings.Cut(line, "{")
		if !ok {
			continue
		}
		m, ok := vmiMetrics[name]
		if !ok {
			continue
		}
		labels, value, ok := strings.Cut(rest, "} ")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(strings.Fields(value)[0], 64)
		if err != nil {
			continue
		}
		var ns, vm string
		for _, l := range labelRe.FindAllStringSubmatch(labels, -1) {
			switch l[1] {
			case "namespace":
				ns = l[2]
			case "name":
				vm = l[2]
			}
		}
		k := ns + "/" + vm
		cs := out[k]
		cs[m.i] += m.sign * v
		out[k] = cs
	}
	return out, sc.Err()
}
