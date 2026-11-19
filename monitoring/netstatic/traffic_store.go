package netstatic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func validateTrafficState(s TrafficState) error {
	policy, err := hex.DecodeString(s.Policy)
	if s.Version != 1 || err != nil || len(policy) != 32 || s.Baselines == nil || s.Interfaces == nil || s.MonthRotate < 0 || s.MonthRotate > 31 || len(s.Events) > 32 || (s.Report != "raw" && s.Report != "billing") {
		return fmt.Errorf("invalid traffic state schema; restore verified backup, never reset silently")
	}
	if s.Window.Interfaces == nil {
		return fmt.Errorf("missing native traffic window")
	}
	var windowRaw, windowBill TrafficBytes
	for name, arr := range s.Window.Interfaces {
		if name == "" {
			return fmt.Errorf("invalid window interface")
		}
		var previous uint64
		for _, td := range arr {
			if td.Timestamp < previous {
				return fmt.Errorf("unordered traffic window")
			}
			previous = td.Timestamp
			var err error
			windowRaw, err = trafficAddBytes(windowRaw, TrafficBytes{In: td.Rx, Out: td.Tx})
			if err != nil {
				return err
			}
			windowBill, err = trafficAddBytes(windowBill, TrafficBytes{In: td.BillingRx, Out: td.BillingTx})
			if err != nil {
				return err
			}
		}
	}
	if windowRaw.In > s.Raw.In || windowRaw.Out > s.Raw.Out || windowBill.In > s.Billing.In || windowBill.Out > s.Billing.Out {
		return fmt.Errorf("native window exceeds lifetime totals")
	}
	for name, b := range s.Baselines {
		if name == "" || b.Identity == "" || s.BootID == "" || s.UpdatedAt.IsZero() {
			return fmt.Errorf("invalid traffic baseline")
		}
	}
	var auditRaw, auditBill TrafficBytes
	for name, a := range s.Interfaces {
		if name == "" {
			return fmt.Errorf("invalid traffic audit interface")
		}
		for _, r := range a.Remainder {
			if r >= TrafficScale {
				return fmt.Errorf("invalid traffic remainder")
			}
		}
		auditRaw, err = trafficAddBytes(auditRaw, a.Raw)
		if err != nil {
			return err
		}
		auditBill, err = trafficAddBytes(auditBill, a.Billing)
		if err != nil {
			return err
		}
	}
	if auditRaw != s.Raw || auditBill != s.Billing {
		return fmt.Errorf("interface audits disagree with lifetime totals")
	}
	return nil
}

// ReadTrafficState 只读取账本，无需加锁或创建目录。
// 原子重命名让离线诊断在 Agent 运行期间也能读到完整的持久化快照。
func ReadTrafficState(path string) (TrafficState, error) {
	var s TrafficState
	st, err := os.Lstat(path)
	if err != nil {
		return s, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return s, fmt.Errorf("traffic state must be a regular private (0600) file")
	}
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	if err = decodeTraffic(f, &s); err != nil {
		return s, fmt.Errorf("corrupt traffic state: %w", err)
	}
	want := s.Checksum
	s.Checksum = ""
	canonical, err := json.Marshal(s)
	if err != nil {
		return s, err
	}
	h := sha256.Sum256(canonical)
	s.Checksum = want
	if want != hex.EncodeToString(h[:]) {
		return s, fmt.Errorf("corrupt traffic state checksum; restore a verified backup")
	}
	return s, validateTrafficState(s)
}

func trafficPrivateDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("traffic state directory must be private (0700), without a directory symlink")
	}
	return nil
}

func OpenTraffic(c TrafficConfig) (*TrafficLedger, error) {
	if err := trafficPrivateDir(c.StateFile); err != nil {
		return nil, err
	}
	lock, err := lockTraffic(c.StateFile + ".lock")
	if err != nil {
		return nil, err
	}
	s, err := ReadTrafficState(c.StateFile)
	if err != nil {
		lock.close()
		return nil, fmt.Errorf("traffic state: %w; initialize explicitly with traffic-seed", err)
	}
	if s.MonthRotate != c.MonthRotate {
		lock.close()
		return nil, fmt.Errorf("month-rotate changed; explicitly seed a new state with reviewed totals")
	}
	if s.Report != c.Report {
		lock.close()
		return nil, fmt.Errorf("report basis changed; explicitly migrate to a new state with reviewed totals")
	}
	return &TrafficLedger{config: c, state: s, lock: lock, lastSave: s.UpdatedAt}, nil
}

type trafficDirectorySyncError struct{ cause error }

func (e *trafficDirectorySyncError) Error() string {
	return fmt.Sprintf("traffic checkpoint published but directory sync failed: %v", e.cause)
}

func (e *trafficDirectorySyncError) Unwrap() error { return e.cause }

func writeTrafficState(path string, s TrafficState) error {
	if err := validateTrafficState(s); err != nil {
		return err
	}
	s.Checksum = ""
	canonical, err := json.Marshal(s)
	if err != nil {
		return err
	}
	h := sha256.Sum256(canonical)
	s.Checksum = hex.EncodeToString(h[:])
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if len(b) >= 64<<20 {
		return fmt.Errorf("traffic state exceeds 64 MiB limit")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".traffic-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if err = d.Sync(); err != nil {
		return &trafficDirectorySyncError{cause: err}
	}
	return nil
}

func (l *TrafficLedger) save() error { return writeTrafficState(l.config.StateFile, l.state) }

// SeedTraffic 只初始化新账本。输入使用已核对的现有累计字节，
// 避免用当前倍率换算操作系统的历史计数。
func SeedTraffic(c TrafficConfig, raw, billing TrafficBytes, nowPeriodRaw, nowPeriodBilling TrafficBytes) error {
	if err := trafficPrivateDir(c.StateFile); err != nil {
		return err
	}
	lock, err := lockTraffic(c.StateFile + ".lock")
	if err != nil {
		return err
	}
	defer lock.close()
	if _, err = os.Lstat(c.StateFile); !os.IsNotExist(err) {
		return fmt.Errorf("traffic seed requires an absent state file")
	}
	s := newTrafficState(c)
	s.Raw, s.Billing = raw, billing
	s.Interfaces["__migration_seed__"] = TrafficInterfaceAudit{Raw: raw, Billing: billing}
	if c.MonthRotate != 0 {
		appendTrafficWindow(&s.Window, "__migration_seed__", TrafficData{Timestamp: uint64(time.Now().Unix()), Rx: nowPeriodRaw.In, Tx: nowPeriodRaw.Out, BillingRx: nowPeriodBilling.In, BillingTx: nowPeriodBilling.Out})
	} else if nowPeriodRaw != (TrafficBytes{}) || nowPeriodBilling != (TrafficBytes{}) {
		return fmt.Errorf("period seeds require the existing month-rotate option")
	}
	// 第一次实际采样只建立计数基线，历史流量保留在原累计中。
	s.event("explicitly seeded; first sample establishes baseline")
	return writeTrafficState(c.StateFile, s)
}
