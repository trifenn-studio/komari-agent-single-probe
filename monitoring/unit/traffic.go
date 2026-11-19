package monitoring

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/komari-monitor/komari-agent/monitoring/netstatic"
)

var trafficRuntime struct {
	sync.Mutex
	ledger    *netstatic.TrafficLedger
	config    netstatic.TrafficConfig
	sampledAt time.Time
}

func StartTraffic() error {
	trafficRuntime.Lock()
	defer trafficRuntime.Unlock()
	if trafficRuntime.ledger != nil {
		return fmt.Errorf("traffic already started")
	}
	c, err := netstatic.LoadTrafficConfig(flags.TrafficConfig, flags.IncludeNics, flags.ExcludeNics, flags.MonthRotate)
	if err != nil {
		return err
	}
	ios, boot, err := netstatic.CollectTraffic(c)
	if err != nil {
		return err
	}
	now := time.Now()
	present := make(map[string]bool, len(ios))
	for _, counter := range ios {
		present[counter.Name] = true
	}
	for _, name := range c.Selected() {
		if !present[name] {
			return fmt.Errorf("strict traffic interface missing: %s", name)
		}
	}
	l, err := netstatic.OpenTraffic(c)
	if err != nil {
		return err
	}
	if _, err = l.Sample(ios, boot, now); err != nil {
		return errors.Join(err, l.Close())
	}
	trafficRuntime.ledger, trafficRuntime.config, trafficRuntime.sampledAt = l, c, now
	return nil
}

func StopTraffic() error {
	trafficRuntime.Lock()
	defer trafficRuntime.Unlock()
	if trafficRuntime.ledger == nil {
		return nil
	}
	err := trafficRuntime.ledger.Close()
	trafficRuntime.ledger = nil
	return err
}

func trafficNetworkSpeed() (totalUp, totalDown, upSpeed, downSpeed uint64, err error) {
	trafficRuntime.Lock()
	defer trafficRuntime.Unlock()
	l := trafficRuntime.ledger
	if l == nil {
		return 0, 0, 0, 0, fmt.Errorf("traffic ledger was not initialized")
	}
	var r netstatic.TrafficResult
	ios, boot, collectErr := netstatic.CollectTraffic(trafficRuntime.config)
	if collectErr == nil {
		now := time.Now()
		r, err = l.Sample(ios, boot, now)
		if err == nil {
			seconds := now.Sub(trafficRuntime.sampledAt).Seconds()
			if seconds > 0 {
				upSpeed = uint64(float64(r.RawDelta.Out) / seconds)
				downSpeed = uint64(float64(r.RawDelta.In) / seconds)
			}
			trafficRuntime.sampledAt = now
			if len(r.Missing) > 0 {
				err = fmt.Errorf("strict traffic interfaces missing: %s", strings.Join(r.Missing, ","))
			}
		}
	} else {
		r, err = l.Result(), collectErr
	}
	totals := r.Raw
	if trafficRuntime.config.Report == "billing" {
		totals = r.Billing
	}
	return totals.Out, totals.In, upSpeed, downSpeed, err
}
