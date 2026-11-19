//go:build !linux

package netstatic

import "fmt"

func CollectTraffic(c TrafficConfig) ([]TrafficCounter, string, error) {
	return nil, "", fmt.Errorf("traffic extension requires Linux boot and interface identities")
}
