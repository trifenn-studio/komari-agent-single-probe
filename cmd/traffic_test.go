package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	flags_pkg "github.com/komari-monitor/komari-agent/cmd/flags"
)

func TestOfflineTrafficConfigNeverLoadsAgentCredentials(t *testing.T) {
	previous := *flags
	defer func() { *flags = previous }()
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.json")
	config := fmt.Sprintf(`{"version":1,"state_file":%q}`, filepath.Join(dir, "state.json"))
	if err := os.WriteFile(policy, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_TRAFFIC_CONFIG", policy)
	t.Setenv("AGENT_INCLUDE_NICS", "eth0")
	t.Setenv("AGENT_EXCLUDE_NICS", "")
	t.Setenv("AGENT_MONTH_ROTATE", "")
	t.Setenv("AGENT_CONFIG_FILE", "/nonexistent/credentials.json")
	t.Setenv("AGENT_TOKEN", "must-not-be-loaded")
	flags.ConfigFile = "/nonexistent/credentials.json"
	flags.Token = ""
	c, err := offlineTrafficConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Report != "raw" || len(c.Selected()) != 1 || flags.Token != "" {
		t.Fatal("offline helper loaded service config")
	}
	t.Setenv("AGENT_MONTH_ROTATE", "invalid")
	if _, err = offlineTrafficConfig(); err == nil {
		t.Fatal("invalid month env accepted")
	}
}

func TestTrafficPolicyPathIsLocal(t *testing.T) {
	previous := *flags
	t.Cleanup(func() {
		*flags = previous
		_ = flags_pkg.CaptureStartupConfig(nil)
	})
	flags.TrafficConfig = filepath.Join(t.TempDir(), "policy.json")
	if err := flags_pkg.CaptureStartupConfig(nil); err != nil {
		t.Fatal(err)
	}
	if _, exists := flags_pkg.StartupConfig()["traffic_config"]; exists {
		t.Fatal("startup report included the local traffic policy path")
	}
}
