package netstatic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const TrafficScale uint64 = 1000000

// TrafficWeights 将物理 RX/TX 字节映射为计费入站和出站字节。
// 权重以整数 ppm 表示，显式规则中省略的权重按零处理。
type TrafficWeights struct {
	RXIn  uint64 `json:"rx_to_in_ppm"`
	RXOut uint64 `json:"rx_to_out_ppm"`
	TXIn  uint64 `json:"tx_to_in_ppm"`
	TXOut uint64 `json:"tx_to_out_ppm"`
}

type TrafficConfig struct {
	Version      int                       `json:"version"`
	StateFile    string                    `json:"state_file"`
	Report       string                    `json:"report"`
	FlushSeconds int                       `json:"flush_seconds"`
	Weights      map[string]TrafficWeights `json:"weights"`
	Include      []string                  `json:"-"`
	Exclude      []string                  `json:"-"`
	MonthRotate  int                       `json:"-"`
}

func decodeTraffic(r io.Reader, dst interface{}) error {
	d := json.NewDecoder(io.LimitReader(r, 64<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra interface{}
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	return nil
}

func trafficNames(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var names []string
	for _, n := range strings.Split(s, ",") {
		n = strings.TrimSpace(n)
		if n == "" || strings.ContainsAny(n, "/*?[]\\ \t\r\n") || n == "." || n == ".." {
			return nil, fmt.Errorf("traffic requires full interface names, got %q", n)
		}
		if !seen[n] {
			names = append(names, n)
			seen[n] = true
		}
	}
	sort.Strings(names)
	return names, nil
}

func LoadTrafficConfig(path, include, exclude string, month int) (TrafficConfig, error) {
	var c TrafficConfig
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	if err = decodeTraffic(f, &c); err != nil {
		return c, fmt.Errorf("traffic config: %w", err)
	}
	if c.Version != 1 {
		return c, fmt.Errorf("traffic config version must be 1")
	}
	if c.Report == "" {
		c.Report = "raw"
	}
	if c.Report != "raw" && c.Report != "billing" {
		return c, fmt.Errorf("traffic report must be raw or billing")
	}
	if !filepath.IsAbs(c.StateFile) {
		return c, fmt.Errorf("traffic state_file must be absolute")
	}
	if c.FlushSeconds == 0 {
		c.FlushSeconds = 60
	}
	if c.FlushSeconds < 10 || c.FlushSeconds > 3600 {
		return c, fmt.Errorf("traffic flush_seconds must be 10..3600")
	}
	if month < 0 || month > 31 {
		return c, fmt.Errorf("month-rotate must be 0..31")
	}
	c.MonthRotate = month
	if c.Include, err = trafficNames(include); err != nil {
		return c, err
	}
	if c.Exclude, err = trafficNames(exclude); err != nil {
		return c, err
	}
	if len(c.Include) == 0 || len(c.Selected()) == 0 {
		return c, fmt.Errorf("traffic requires a nonempty --include-nics after exclusions")
	}
	for name, w := range c.Weights {
		if !slices.Contains(c.Include, name) {
			return c, fmt.Errorf("weight interface %q is absent from include-nics", name)
		}
		for _, v := range []uint64{w.RXIn, w.RXOut, w.TXIn, w.TXOut} {
			if v > 1000000000000 {
				return c, fmt.Errorf("weight for %s exceeds 1000000000000 ppm", name)
			}
		}
	}
	return c, nil
}

func (c TrafficConfig) Selected() []string {
	names := []string{}
	for _, n := range c.Include {
		if !slices.Contains(c.Exclude, n) {
			names = append(names, n)
		}
	}
	return names
}

func (c TrafficConfig) Weight(name string) TrafficWeights {
	if w, ok := c.Weights[name]; ok {
		return w
	}
	return TrafficWeights{RXIn: TrafficScale, TXOut: TrafficScale}
}

func (c TrafficConfig) fingerprint() string {
	// 状态文件路径和落盘频率独立于计量策略。
	b, _ := json.Marshal(struct {
		Include, Exclude []string
		Weights          map[string]TrafficWeights
	}{c.Include, c.Exclude, c.Weights})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
