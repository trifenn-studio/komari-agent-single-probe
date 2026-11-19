package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/komari-monitor/komari-agent/monitoring/netstatic"
	"github.com/spf13/cobra"
)

// Diagnostics deliberately load only traffic-related env and the separate policy
// file, never --config / AGENT_CONFIG_FILE, token, discovery, or server state.
func offlineTrafficConfig() (netstatic.TrafficConfig, error) {
	path, include, exclude, month := flags.TrafficConfig, flags.IncludeNics, flags.ExcludeNics, flags.MonthRotate
	if v := os.Getenv("AGENT_TRAFFIC_CONFIG"); v != "" {
		path = v
	}
	if v := os.Getenv("AGENT_INCLUDE_NICS"); v != "" {
		include = v
	}
	if v := os.Getenv("AGENT_EXCLUDE_NICS"); v != "" {
		exclude = v
	}
	if v := os.Getenv("AGENT_MONTH_ROTATE"); v != "" {
		var err error
		month, err = strconv.Atoi(v)
		if err != nil {
			return netstatic.TrafficConfig{}, fmt.Errorf("invalid AGENT_MONTH_ROTATE: %w", err)
		}
	}
	if path == "" {
		return netstatic.TrafficConfig{}, fmt.Errorf("--traffic-config is required")
	}
	return netstatic.LoadTrafficConfig(path, include, exclude, month)
}

func trafficJSON(cmd *cobra.Command, value interface{}) error {
	e := json.NewEncoder(cmd.OutOrStdout())
	e.SetIndent("", "  ")
	return e.Encode(value)
}

func init() {
	check := &cobra.Command{
		Use:   "traffic-check",
		Short: "Check traffic policy and live interfaces offline (read only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := offlineTrafficConfig()
			if err != nil {
				return err
			}
			ios, _, err := netstatic.CollectTraffic(c)
			if err != nil {
				return err
			}
			type nic struct {
				Name    string                    `json:"name"`
				Present bool                      `json:"present"`
				Weights netstatic.TrafficWeights  `json:"weights"`
				Counter *netstatic.TrafficCounter `json:"counter,omitempty"`
			}
			nics := []nic{}
			missing := []string{}
			counters := make(map[string]*netstatic.TrafficCounter, len(ios))
			for i := range ios {
				counters[ios[i].Name] = &ios[i]
			}
			for _, name := range c.Selected() {
				counter := counters[name]
				n := nic{Name: name, Weights: c.Weight(name), Present: counter != nil, Counter: counter}
				if !n.Present {
					missing = append(missing, name)
				}
				nics = append(nics, n)
			}
			if err := trafficJSON(cmd, map[string]any{
				"version": 1, "report": c.Report, "unit": "bytes", "rate_unit": "bytes/second",
				"month_rotate": c.MonthRotate, "state_file": c.StateFile,
				"excluded": c.Exclude, "interfaces": nics, "missing": missing, "ok": len(missing) == 0,
			}); err != nil {
				return err
			}
			if len(missing) > 0 {
				return fmt.Errorf("strict traffic interfaces missing: %v", missing)
			}
			return nil
		},
	}
	status := &cobra.Command{
		Use:   "traffic-status",
		Short: "Read persisted traffic audit offline (read only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := offlineTrafficConfig()
			if err != nil {
				return err
			}
			s, err := netstatic.ReadTrafficState(c.StateFile)
			if err != nil {
				return err
			}
			return trafficJSON(cmd, s)
		},
	}
	var raw, billing, periodRaw, periodBilling netstatic.TrafficBytes
	seed := &cobra.Command{
		Use:   "traffic-seed",
		Short: "Create a new traffic ledger from explicit byte totals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := offlineTrafficConfig()
			if err != nil {
				return err
			}
			if c.MonthRotate != 0 {
				for _, name := range []string{"period-raw-in", "period-raw-out", "period-billing-in", "period-billing-out"} {
					if !cmd.Flags().Changed(name) {
						return fmt.Errorf("existing month-rotate requires explicit --%s", name)
					}
				}
			}
			if err := netstatic.SeedTraffic(c, raw, billing, periodRaw, periodBilling); err != nil {
				return err
			}
			return trafficJSON(cmd, map[string]any{
				"version": 1, "created": c.StateFile, "baseline": "first successful live sample", "unit": "bytes",
			})
		},
	}
	for _, p := range []struct {
		name string
		dst  *uint64
	}{
		{"raw-in", &raw.In}, {"raw-out", &raw.Out}, {"billing-in", &billing.In}, {"billing-out", &billing.Out},
		{"period-raw-in", &periodRaw.In}, {"period-raw-out", &periodRaw.Out},
		{"period-billing-in", &periodBilling.In}, {"period-billing-out", &periodBilling.Out},
	} {
		seed.Flags().Uint64Var(p.dst, p.name, 0, "Reviewed seed in bytes")
		if !strings.HasPrefix(p.name, "period-") {
			_ = seed.MarkFlagRequired(p.name)
		}
	}
	RootCmd.AddCommand(check, status, seed)
}
