//go:build windows

package netstatic

import "fmt"

type trafficLock struct{}

func lockTraffic(path string) (*trafficLock, error) {
	return nil, fmt.Errorf("traffic extension requires Linux")
}
func (l *trafficLock) close() error { return nil }
