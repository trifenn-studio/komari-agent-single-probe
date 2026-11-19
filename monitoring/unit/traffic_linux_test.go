//go:build linux

package monitoring

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/komari-monitor/komari-agent/monitoring/netstatic"
)

func TestTrafficNetworkSpeedUsesLiveRawRatesAndRestarts(t *testing.T) {
	dir := t.TempDir()
	previous := *flags
	t.Cleanup(func() {
		if err := StopTraffic(); err != nil {
			t.Error(err)
		}
		*flags = previous
	})
	t.Setenv("HOST_PROC", "/proc")
	flags.TrafficConfig = filepath.Join(dir, "policy.json")
	flags.IncludeNics, flags.ExcludeNics, flags.MonthRotate = "lo,excluded-fixture", "excluded-fixture", 1
	policy := netstatic.TrafficConfig{
		Version: 1, StateFile: filepath.Join(dir, "ledger", "state.json"), Report: "billing",
		Weights: map[string]netstatic.TrafficWeights{"lo": {TXOut: 2 * netstatic.TrafficScale}},
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flags.TrafficConfig, data, 0600); err != nil {
		t.Fatal(err)
	}
	config, err := netstatic.LoadTrafficConfig(flags.TrafficConfig, flags.IncludeNics, flags.ExcludeNics, flags.MonthRotate)
	if err != nil {
		t.Fatal(err)
	}
	seed := netstatic.TrafficBytes{In: 100, Out: 200}
	if err := netstatic.SeedTraffic(config, seed, seed, seed, seed); err != nil {
		t.Fatal(err)
	}
	if err := StartTraffic(); err != nil {
		t.Fatal(err)
	}
	if names, err := InterfaceList(); err != nil || !reflect.DeepEqual(names, []string{"lo"}) {
		t.Fatalf("monitored interfaces = %v, err = %v", names, err)
	}

	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	data = make([]byte, 1024)
	if _, err := listener.WriteToUDP(data, listener.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}

	start := trafficRuntime.sampledAt
	out, in, sendRate, receiveRate, err := NetworkSpeed()
	elapsed := trafficRuntime.sampledAt.Sub(start).Seconds()
	if err != nil || out < seed.Out+2*uint64(len(data)) || in != seed.In || sendRate == 0 {
		t.Fatalf("billing totals = (%d out, %d in), rates = (%d up, %d down), err = %v", out, in, sendRate, receiveRate, err)
	}
	rawDelta := (out - seed.Out) / 2
	if want := uint64(float64(rawDelta) / elapsed); sendRate != want || receiveRate != want {
		t.Fatalf("rates = (%d up, %d down), want %d byte/s from real loopback bytes", sendRate, receiveRate, want)
	}
	if err := StopTraffic(); err != nil {
		t.Fatal(err)
	}
	persisted, err := netstatic.ReadTrafficState(config.StateFile)
	if err != nil || persisted.Raw.Out != seed.Out+rawDelta || persisted.Billing.Out != out || persisted.Billing.In != in {
		t.Fatalf("wrong totals in persisted ledger: %v", err)
	}
	if err := StartTraffic(); err != nil {
		t.Fatal(err)
	}
	nextOut, nextIn, _, _, err := NetworkSpeed()
	if err != nil || nextOut < out || nextIn != in {
		t.Fatalf("restarted totals = (%d out, %d in), previously (%d out, %d in), err = %v", nextOut, nextIn, out, in, err)
	}
}
