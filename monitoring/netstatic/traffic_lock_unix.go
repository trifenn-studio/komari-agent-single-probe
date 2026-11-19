//go:build !windows

package netstatic

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type trafficLock struct{ f *os.File }

func lockTraffic(path string) (*trafficLock, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("traffic state is locked by another instance: %w", err)
	}
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	return &trafficLock{f: f}, nil
}

func (l *trafficLock) close() error { return l.f.Close() }
