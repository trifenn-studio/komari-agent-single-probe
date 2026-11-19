package netstatic

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testTrafficConfig(t *testing.T) TrafficConfig {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ledger")
	return TrafficConfig{Version: 1, StateFile: filepath.Join(dir, "traffic.json"), Report: "raw", FlushSeconds: 60, Include: []string{"P", "R"}, Weights: map[string]TrafficWeights{
		"P": {RXIn: TrafficScale, TXOut: TrafficScale}, "R": {RXOut: TrafficScale, TXOut: 2 * TrafficScale},
	}}
}
func counter(name string, rx, tx uint64) TrafficCounter {
	return TrafficCounter{Name: name, Identity: "1/aa", RX: rx, TX: tx}
}
func applyTest(t *testing.T, c TrafficConfig, s TrafficState, ios []TrafficCounter, boot string, now time.Time) (TrafficState, TrafficResult) {
	t.Helper()
	s, r, err := applyTraffic(c, s, ios, boot, now)
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}
func requireTrafficLedgerStore(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the traffic ledger writer requires POSIX file locking")
	}
}

func TestTrafficDirectionalMappingAndPolicyChange(t *testing.T) {
	c := testTrafficConfig(t)
	now := time.Now()
	s := newTrafficState(c)
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 10000, 20000), counter("R", 30000, 40000)}, "boot", now)
	s, r := applyTest(t, c, s, []TrafficCounter{counter("P", 10100, 20200), counter("R", 30300, 40400)}, "boot", now.Add(time.Second))
	if r.Raw != (TrafficBytes{400, 600}) || r.Billing != (TrafficBytes{100, 1300}) {
		t.Fatalf("mapping: %+v", r)
	}
	c.Weights["R"] = TrafficWeights{RXIn: 500000, TXIn: 500000, TXOut: 500000, RXOut: 500000}
	before := s.Billing
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 99999, 99999), counter("R", 99999, 99999)}, "boot", now.Add(2*time.Second))
	if s.Billing != before {
		t.Fatal("policy change recalculated history")
	}
	s, r = applyTest(t, c, s, []TrafficCounter{counter("P", 100001, 100001), counter("R", 100003, 100005)}, "boot", now.Add(3*time.Second))
	if s.Billing != (TrafficBytes{107, 1307}) || r.RawDelta != (TrafficBytes{6, 8}) {
		t.Fatalf("four directions: %+v", r)
	}
}
func TestTrafficFractionSegmentationAndRestart(t *testing.T) {
	c := testTrafficConfig(t)
	c.Include = []string{"P"}
	c.Weights = map[string]TrafficWeights{"P": {RXIn: 500000, TXOut: 500000}}
	now := time.Now()
	s := newTrafficState(c)
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 0, 0)}, "boot", now)
	split := s
	for i := uint64(1); i <= 101; i++ {
		split, _ = applyTest(t, c, split, []TrafficCounter{counter("P", i, i)}, "boot", now.Add(time.Duration(i)*time.Second))
	}
	one, _ := applyTest(t, c, s, []TrafficCounter{counter("P", 101, 101)}, "boot", now.Add(101*time.Second))
	if one.Billing != split.Billing || one.Interfaces["P"].Remainder != split.Interfaces["P"].Remainder || one.Billing.In != 50 {
		t.Fatal("sample partition changed fixed point totals")
	}
	requireTrafficLedgerStore(t)
	if err := trafficPrivateDir(c.StateFile); err != nil {
		t.Fatal(err)
	}
	if err := writeTrafficState(c.StateFile, split); err != nil {
		t.Fatal(err)
	}
	l, err := OpenTraffic(c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := l.Sample([]TrafficCounter{counter("P", 102, 102)}, "boot", now.Add(102*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if r.Billing != (TrafficBytes{51, 51}) {
		t.Fatalf("restart lost remainder: %+v", r)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	persisted, err := ReadTrafficState(c.StateFile)
	if err != nil || persisted.Billing != r.Billing {
		t.Fatalf("persisted: %v", err)
	}
}
func TestTrafficResetsMissingIdentityAndBoot(t *testing.T) {
	c := testTrafficConfig(t)
	c.Include = []string{"P"}
	now := time.Now()
	s := newTrafficState(c)
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 100, 100)}, "a", now)
	s, r := applyTest(t, c, s, []TrafficCounter{counter("P", 10, 150)}, "a", now.Add(time.Second))
	if r.Raw != (TrafficBytes{0, 50}) {
		t.Fatal(r)
	}
	s, r = applyTest(t, c, s, nil, "a", now.Add(2*time.Second))
	if len(r.Missing) != 1 {
		t.Fatal("missing not diagnosed")
	}
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 999999, 999999)}, "a", now.Add(3*time.Second))
	if s.Raw != (TrafficBytes{0, 50}) {
		t.Fatal("reappearance spike")
	}
	changed := counter("P", math.MaxUint64, math.MaxUint64)
	changed.Identity = "2/bb"
	s, _ = applyTest(t, c, s, []TrafficCounter{changed}, "a", now.Add(4*time.Second))
	if s.Raw.Out != 50 {
		t.Fatal("identity spike")
	}
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 0, 0)}, "b", now.Add(5*time.Second))
	if s.Raw.Out != 50 {
		t.Fatal("boot spike")
	}
	// 接口改名后视为缺失，直到显式选中新名称。
	s, r = applyTest(t, c, s, []TrafficCounter{counter("new-name", 999999, 999999)}, "b", now.Add(6*time.Second))
	if len(r.Missing) != 1 || s.Raw.Out != 50 {
		t.Fatal("rename fallback")
	}
}
func TestTrafficOverflowIsTransactional(t *testing.T) {
	c := testTrafficConfig(t)
	c.Include = []string{"P"}
	c.Weights["P"] = TrafficWeights{RXIn: 2 * TrafficScale}
	now := time.Now()
	s := newTrafficState(c)
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 0, 0)}, "b", now)
	snapshot := cloneTrafficState(s)
	_, _, err := applyTraffic(c, s, []TrafficCounter{counter("P", math.MaxUint64, 1)}, "b", now.Add(time.Second))
	if err == nil || !reflect.DeepEqual(s, snapshot) {
		t.Fatal("overflow committed partial state")
	}
	s.Raw.In = math.MaxUint64
	_, _, err = applyTraffic(c, s, []TrafficCounter{counter("P", 1, 1)}, "b", now.Add(time.Second))
	if err == nil {
		t.Fatal("raw total overflow")
	}
	_, _, err = applyTraffic(c, s, []TrafficCounter{counter("P", 1, 1)}, "b", now.Add(-time.Second))
	if err == nil {
		t.Fatal("backwards clock")
	}
}
func TestTrafficStoreLockCorruptionSeedAndPermissions(t *testing.T) {
	requireTrafficLedgerStore(t)
	c := testTrafficConfig(t)
	if _, err := OpenTraffic(c); err == nil {
		t.Fatal("unseeded ledger accepted")
	}
	if err := SeedTraffic(c, TrafficBytes{100, 200}, TrafficBytes{100, 200}, TrafficBytes{}, TrafficBytes{}); err != nil {
		t.Fatal(err)
	}
	if err := SeedTraffic(c, TrafficBytes{}, TrafficBytes{}, TrafficBytes{}, TrafficBytes{}); err == nil {
		t.Fatal("seed overwrote existing ledger")
	}
	l, err := OpenTraffic(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTraffic(c); err == nil {
		t.Fatal("second writer acquired lock")
	}
	// 离线状态查询无需加锁，保持文件字节和修改时间。
	before, _ := os.ReadFile(c.StateFile)
	st, _ := os.Stat(c.StateFile)
	if _, err = ReadTrafficState(c.StateFile); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(c.StateFile)
	st2, _ := os.Stat(c.StateFile)
	if string(before) != string(after) || !st.ModTime().Equal(st2.ModTime()) {
		t.Fatal("status mutated ledger")
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	st, _ = os.Stat(c.StateFile)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	for _, bad := range []string{"", "{", "{}", `{"version":1,"unexpected":true}`} {
		if err = os.WriteFile(c.StateFile, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = OpenTraffic(c); err == nil {
			t.Fatalf("corruption accepted: %q", bad)
		}
		bytes, _ := os.ReadFile(c.StateFile)
		if string(bytes) != bad {
			t.Fatal("corruption was reset")
		}
	}
	if err = os.Chmod(c.StateFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadTrafficState(c.StateFile); err == nil {
		t.Fatal("public state accepted")
	}
}
func TestTrafficFlushCadenceAndPersistenceFailure(t *testing.T) {
	requireTrafficLedgerStore(t)
	c := testTrafficConfig(t)
	c.Include = []string{"P"}
	if err := SeedTraffic(c, TrafficBytes{}, TrafficBytes{}, TrafficBytes{}, TrafficBytes{}); err != nil {
		t.Fatal(err)
	}
	l, err := OpenTraffic(c)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	now := time.Now()
	if _, err = l.Sample([]TrafficCounter{counter("P", 0, 0)}, "b", now); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(c.StateFile)
	if _, err = l.Sample([]TrafficCounter{counter("P", 10, 20)}, "b", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(c.StateFile)
	if string(before) != string(after) {
		t.Fatal("wrote ledger every sample")
	}
	if err = os.Remove(c.StateFile); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(c.StateFile, 0700); err != nil {
		t.Fatal(err)
	}
	accepted := cloneTrafficState(l.state)
	failedResult, err := l.Sample([]TrafficCounter{counter("P", 20, 30)}, "b", now.Add(61*time.Second))
	if err == nil {
		t.Fatal("save failure swallowed")
	}
	if failedResult.Raw != accepted.Raw || failedResult.Billing != accepted.Billing || !reflect.DeepEqual(l.state, accepted) {
		t.Fatal("failed checkpoint exposed or accepted candidate totals", failedResult)
	}
	beforeRaw := l.state.Raw
	if _, err = l.Sample([]TrafficCounter{counter("P", 100, 100)}, "b", now.Add(62*time.Second)); err == nil || l.state.Raw != beforeRaw {
		t.Fatal("writer continued after persistence failure")
	}
	// 正常关闭前恢复文件系统，只保存已接受的冻结状态。
	// 再次打开时，被拒绝的区间只计入一次。
	if err = os.Remove(c.StateFile); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	persisted, err := ReadTrafficState(c.StateFile)
	if err != nil || persisted.Raw != accepted.Raw || !persisted.UpdatedAt.Equal(accepted.UpdatedAt) {
		t.Fatal("close saved rejected candidate", err)
	}
	if _, err = l.Sample([]TrafficCounter{counter("P", 100, 100)}, "b", now.Add(63*time.Second)); err == nil {
		t.Fatal("closed ledger accepted sample")
	}
	reopened, err := OpenTraffic(c)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := reopened.Sample([]TrafficCounter{counter("P", 20, 30)}, "b", now.Add(64*time.Second))
	if err != nil || recovered.Raw != (TrafficBytes{20, 30}) {
		t.Fatal("recovery duplicated or lost rejected interval", recovered, err)
	}
}
func TestTrafficNativeWindowBoundaryAndCache(t *testing.T) {
	c := testTrafficConfig(t)
	c.Include = []string{"P"}
	c.MonthRotate = 1
	// 沿用现有查询函数，并包含原生查询区间的两端。
	midnight := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	s := newTrafficState(c)
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 100, 100)}, "b", midnight.Add(-2*time.Second))
	s, r := applyTest(t, c, s, []TrafficCounter{counter("P", 110, 120)}, "b", midnight.Add(-time.Second))
	if r.Raw != (TrafficBytes{10, 20}) {
		t.Fatal(r)
	}
	s, r = applyTest(t, c, s, []TrafficCounter{counter("P", 115, 125)}, "b", midnight)
	if r.Raw != (TrafficBytes{5, 5}) || s.Raw != (TrafficBytes{15, 25}) {
		t.Fatalf("month boundary: %+v", r)
	}
	data := totalTrafficBetween(s.Window.Interfaces, map[string][]TrafficData{"P": {{Timestamp: uint64(midnight.Unix()), Tx: 7, Rx: 8}}}, uint64(midnight.Unix()), uint64(midnight.Unix()))
	if data["P"].Tx != 12 || data["P"].Rx != 13 {
		t.Fatalf("cache/boundary: %+v", data)
	}
	c.MonthRotate = 31
	// 原生日期计算将 2 月 31 日顺延为 3 月 1 日。
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.Local)
	s = newTrafficState(c)
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 0, 0)}, "b", at.Add(-time.Second))
	_, r = applyTest(t, c, s, []TrafficCounter{counter("P", 9, 9)}, "b", at)
	if r.Raw.In != 9 {
		t.Fatal(r)
	}
}
func TestTrafficConfigValidationAndExcludePriority(t *testing.T) {
	c := testTrafficConfig(t)
	file := filepath.Join(t.TempDir(), "policy.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	parsed, err := LoadTrafficConfig(file, "P,R", "R", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Selected(), []string{"P"}) {
		t.Fatal("exclude did not win")
	}
	if _, err = LoadTrafficConfig(file, "P*", "", 0); err == nil {
		t.Fatal("wildcard accepted in strict mode")
	}
	if _, err = LoadTrafficConfig(file, "", "", 0); err == nil {
		t.Fatal("empty include accepted")
	}
	for _, value := range []string{"-1", "0.5", "NaN", "18446744073709551616", "1000000000001"} {
		invalid := fmt.Sprintf(`{"version":1,"state_file":%q,"weights":{"P":{"rx_to_in_ppm":%s}}}`, c.StateFile, value)
		if err := os.WriteFile(file, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = LoadTrafficConfig(file, "P", "", 0); err == nil {
			t.Fatalf("invalid weight %s accepted", value)
		}
	}
	if _, err = trafficNames("P,,R"); err == nil {
		t.Fatal("empty interface accepted")
	}
	if _, err = trafficNames("tun0"); err != nil {
		t.Fatal(err)
	}
	if _, err = trafficNames("a/b"); err == nil || !strings.Contains(err.Error(), "full interface") {
		t.Fatal(err)
	}
}

func TestTrafficEarnedFractionsSurviveMissingAndPolicyChange(t *testing.T) {
	c := testTrafficConfig(t)
	c.Include = []string{"P"}
	c.Weights = map[string]TrafficWeights{"P": {RXIn: 500000}}
	now := time.Now()
	s := newTrafficState(c)
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 0, 0)}, "b", now)
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 1, 0)}, "b", now.Add(time.Second))
	s, _ = applyTest(t, c, s, nil, "b", now.Add(2*time.Second))
	c.Weights["P"] = TrafficWeights{RXIn: 250000}
	s, _ = applyTest(t, c, s, []TrafficCounter{counter("P", 100, 100)}, "b", now.Add(3*time.Second))
	s, r := applyTest(t, c, s, []TrafficCounter{counter("P", 102, 100)}, "b", now.Add(4*time.Second))
	if r.Billing.In != 1 || s.Interfaces["P"].Remainder[0] != 0 {
		t.Fatal("earned fractional credit was discarded", r)
	}
}

func TestTrafficChecksumBasisAndSymlinkProtection(t *testing.T) {
	requireTrafficLedgerStore(t)
	c := testTrafficConfig(t)
	if err := SeedTraffic(c, TrafficBytes{1, 2}, TrafficBytes{1, 2}, TrafficBytes{}, TrafficBytes{}); err != nil {
		t.Fatal(err)
	}
	changed := c
	changed.Report = "billing"
	if _, err := OpenTraffic(changed); err == nil {
		t.Fatal("report basis changed silently")
	}
	b, err := os.ReadFile(c.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	altered := strings.Replace(string(b), `"in":1`, `"in":9`, 1)
	if err = os.WriteFile(c.StateFile, []byte(altered), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadTrafficState(c.StateFile); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("valid JSON corruption accepted: %v", err)
	}
	if err = os.Remove(c.StateFile + ".lock"); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(c.StateFile), "target")
	if err = os.WriteFile(target, []byte("do not touch"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, c.StateFile+".lock"); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenTraffic(c); err == nil {
		t.Fatal("symlink lock accepted")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "do not touch" {
		t.Fatal("symlink target mutated")
	}
}

func TestTrafficFailedCloseReleasesLock(t *testing.T) {
	requireTrafficLedgerStore(t)
	c := testTrafficConfig(t)
	if err := SeedTraffic(c, TrafficBytes{}, TrafficBytes{}, TrafficBytes{}, TrafficBytes{}); err != nil {
		t.Fatal(err)
	}
	l, err := OpenTraffic(c)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(c.StateFile); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(c.StateFile, 0700); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err == nil {
		t.Fatal("failed close swallowed persistence error")
	}
	lock, err := lockTraffic(c.StateFile + ".lock")
	if err != nil {
		t.Fatal("failed close retained process lock", err)
	}
	if err = lock.close(); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal("second close must be idempotent", err)
	}
}
