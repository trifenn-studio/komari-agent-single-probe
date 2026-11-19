//go:build linux

package netstatic

import (
	"fmt"
	snet "net"
	"os"
	"slices"
	"strings"

	gnet "github.com/shirou/gopsutil/v4/net"
)

func trafficIdentities() (map[string]string, error) {
	nics, err := snet.Interfaces()
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, n := range nics {
		m[n.Name] = fmt.Sprintf("%d/%s", n.Index, n.HardwareAddr.String())
	}
	return m, nil
}

func CollectTraffic(c TrafficConfig) ([]TrafficCounter, string, error) {
	// gopsutil 的 HOST_PROC 和 net.Interfaces 必须读取同一个网络命名空间。
	if p := os.Getenv("HOST_PROC"); p != "" && p != "/proc" {
		return nil, "", fmt.Errorf("traffic extension requires native network namespace; HOST_PROC must be /proc or unset")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, "", err
	}
	before, err := trafficIdentities()
	if err != nil {
		return nil, "", err
	}
	ios, err := gnet.IOCounters(true)
	if err != nil {
		return nil, "", err
	}
	if len(ios) == 0 {
		return nil, "", fmt.Errorf("no network counters returned")
	}
	after, err := trafficIdentities()
	if err != nil {
		return nil, "", err
	}
	out := []TrafficCounter{}
	selected := c.Selected()
	for _, cur := range ios {
		if !slices.Contains(selected, cur.Name) {
			continue
		}
		id := after[cur.Name]
		if id == "" || id != before[cur.Name] {
			return nil, "", fmt.Errorf("interface %s changed during collection", cur.Name)
		}
		out = append(out, TrafficCounter{Name: cur.Name, Identity: id, RX: cur.BytesRecv, TX: cur.BytesSent})
	}
	for _, n := range selected {
		if before[n] != after[n] {
			return nil, "", fmt.Errorf("interface %s changed during collection", n)
		}
	}
	return out, strings.TrimSpace(string(boot)), nil
}
