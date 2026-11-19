package netstatic

import (
	"errors"
	"fmt"
	"maps"
	"math/bits"
	"slices"
	"sync"
	"time"

	"github.com/komari-monitor/komari-agent/utils"
)

type TrafficBytes struct {
	In  uint64 `json:"in"`
	Out uint64 `json:"out"`
}

type TrafficCounter struct {
	Name     string `json:"name"`
	Identity string `json:"identity"`
	RX       uint64 `json:"rx"`
	TX       uint64 `json:"tx"`
}

type TrafficBaseline struct {
	Identity string `json:"identity"`
	RX       uint64 `json:"rx"`
	TX       uint64 `json:"tx"`
}

// TrafficState 持久化连续累计和原有的 netstatic 时间窗口。
// 新倍率只适用于后续增量，历史累计保持原值。
type TrafficState struct {
	Version     int                        `json:"version"`
	Checksum    string                     `json:"checksum_sha256"`
	Policy      string                     `json:"policy_sha256"`
	BootID      string                     `json:"boot_id"`
	MonthRotate int                        `json:"month_rotate"`
	Report      string                     `json:"report"`
	UpdatedAt   time.Time                  `json:"updated_at"`
	Raw         TrafficBytes               `json:"raw_bytes"`
	Billing     TrafficBytes               `json:"billing_bytes"`
	Baselines   map[string]TrafficBaseline `json:"baselines"`
	Window      NetStatic                  `json:"netstatic"`
	// 接口被排除或消失后仍保留。
	Interfaces map[string]TrafficInterfaceAudit `json:"interfaces"`
	Events     []string                         `json:"events"`
}

type TrafficInterfaceAudit struct {
	Raw       TrafficBytes `json:"raw_bytes"`
	Billing   TrafficBytes `json:"billing_bytes"`
	Remainder [4]uint64    `json:"remainder_ppm"`
}

type TrafficResult struct {
	Raw      TrafficBytes `json:"raw_bytes"`
	Billing  TrafficBytes `json:"billing_bytes"`
	RawDelta TrafficBytes `json:"raw_delta_bytes"`
	Missing  []string     `json:"missing"`
}

type TrafficLedger struct {
	mu       sync.Mutex
	config   TrafficConfig
	state    TrafficState
	lock     *trafficLock
	lastSave time.Time
	closed   bool
	fatal    error
}

func newTrafficState(c TrafficConfig) TrafficState {
	return TrafficState{Version: 1, Policy: c.fingerprint(), MonthRotate: c.MonthRotate, Report: c.Report,
		Baselines: map[string]TrafficBaseline{}, Interfaces: map[string]TrafficInterfaceAudit{}, Window: NetStatic{Interfaces: map[string][]TrafficData{}, Config: configOrDefault(NetStaticConfig{})}, Events: []string{}}
}

func trafficAdd(a, b uint64) (uint64, error) {
	v, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return 0, fmt.Errorf("traffic uint64 overflow")
	}
	return v, nil
}

func trafficAddBytes(a, b TrafficBytes) (TrafficBytes, error) {
	var err error
	if a.In, err = trafficAdd(a.In, b.In); err != nil {
		return a, err
	}
	a.Out, err = trafficAdd(a.Out, b.Out)
	return a, err
}

func trafficWeighted(delta, weight, remainder uint64) (uint64, uint64, error) {
	hi, lo := bits.Mul64(delta, weight)
	lo, carry := bits.Add64(lo, remainder, 0)
	hi, overflow := bits.Add64(hi, 0, carry)
	if overflow != 0 || hi >= TrafficScale {
		return 0, 0, fmt.Errorf("traffic weighted byte overflow")
	}
	q, r := bits.Div64(hi, lo, TrafficScale)
	return q, r, nil
}

func cloneTrafficState(s TrafficState) TrafficState {
	s.Baselines = maps.Clone(s.Baselines)
	s.Interfaces = maps.Clone(s.Interfaces)
	s.Window.Interfaces = maps.Clone(s.Window.Interfaces)
	for name, records := range s.Window.Interfaces {
		s.Window.Interfaces[name] = slices.Clone(records)
	}
	s.Events = slices.Clone(s.Events)
	return s
}

func (s *TrafficState) event(e string) {
	s.Events = append(s.Events, e)
	if len(s.Events) > 32 {
		s.Events = s.Events[len(s.Events)-32:]
	}
}

// applyTraffic 在样本无效或计算溢出时保留调用方的原有状态。
// 计数器重置只影响该物理方向。
func applyTraffic(c TrafficConfig, old TrafficState, counters []TrafficCounter, boot string, now time.Time) (TrafficState, TrafficResult, error) {
	s := cloneTrafficState(old)
	result := TrafficResult{Missing: []string{}}
	if boot == "" {
		return old, result, fmt.Errorf("empty boot identity")
	}
	if !s.UpdatedAt.IsZero() && now.Before(s.UpdatedAt) {
		return old, result, fmt.Errorf("traffic clock moved backwards")
	}
	policy := c.fingerprint()
	if s.Policy != policy || s.BootID != boot {
		s.Baselines = map[string]TrafficBaseline{}
		s.event("policy or boot identity changed: rebaseline")
	}
	s.Policy, s.BootID = policy, boot
	seen := map[string]TrafficCounter{}
	for _, cur := range counters {
		if cur.Identity == "" || cur.Name == "" {
			return old, result, fmt.Errorf("empty interface identity")
		}
		if _, ok := seen[cur.Name]; ok {
			return old, result, fmt.Errorf("duplicate interface %s", cur.Name)
		}
		seen[cur.Name] = cur
	}
	selected := c.Selected()
	for _, name := range selected {
		cur, ok := seen[name]
		if !ok {
			result.Missing = append(result.Missing, name)
			if _, existed := s.Baselines[name]; existed {
				s.event(name + ": disappeared; rebaseline on return")
			}
			delete(s.Baselines, name)
			continue
		}
		prev, exists := s.Baselines[name]
		if !exists || prev.Identity != cur.Identity {
			s.Baselines[name] = TrafficBaseline{Identity: cur.Identity, RX: cur.RX, TX: cur.TX}
			s.event(name + ": new interface identity/baseline")
			continue
		}
		rx, tx := safeDelta(cur.RX, prev.RX), safeDelta(cur.TX, prev.TX)
		if cur.RX < prev.RX {
			s.event(name + ": RX reset")
		}
		if cur.TX < prev.TX {
			s.event(name + ": TX reset")
		}
		w := c.Weight(name)
		audit := s.Interfaces[name]
		weights := [4]uint64{w.RXIn, w.RXOut, w.TXIn, w.TXOut}
		deltas := [4]uint64{rx, rx, tx, tx}
		var weighted [4]uint64
		for i := range weighted {
			var err error
			weighted[i], audit.Remainder[i], err = trafficWeighted(deltas[i], weights[i], audit.Remainder[i])
			if err != nil {
				return old, result, err
			}
		}
		in, err := trafficAdd(weighted[0], weighted[2])
		if err != nil {
			return old, result, err
		}
		out, err := trafficAdd(weighted[1], weighted[3])
		if err != nil {
			return old, result, err
		}
		raw, bill := TrafficBytes{In: rx, Out: tx}, TrafficBytes{In: in, Out: out}
		for _, p := range []struct {
			dst *TrafficBytes
			add TrafficBytes
		}{
			{&s.Raw, raw}, {&s.Billing, bill},
			{&result.RawDelta, raw}, {&audit.Raw, raw}, {&audit.Billing, bill},
		} {
			*p.dst, err = trafficAddBytes(*p.dst, p.add)
			if err != nil {
				return old, result, err
			}
		}
		if c.MonthRotate != 0 {
			appendTrafficWindow(&s.Window, name, TrafficData{Timestamp: uint64(now.Unix()), Rx: rx, Tx: tx, BillingRx: in, BillingTx: out})
		}
		s.Interfaces[name] = audit
		prev.Identity, prev.RX, prev.TX = cur.Identity, cur.RX, cur.TX
		s.Baselines[name] = prev
	}
	// 移除已取消选择的接口基线，保留其累计用于核对。
	for name := range s.Baselines {
		if !slices.Contains(selected, name) {
			delete(s.Baselines, name)
		}
	}
	s.UpdatedAt = now
	purgeTrafficWindow(&s.Window, now)
	result.Raw, result.Billing = trafficWindowResult(c, s, now)
	return s, result, nil
}

func (l *TrafficLedger) Sample(counters []TrafficCounter, boot string, now time.Time) (TrafficResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return TrafficResult{}, fmt.Errorf("traffic ledger closed")
	}
	if l.fatal != nil {
		return l.result(), l.fatal
	}
	s, r, err := applyTraffic(l.config, l.state, counters, boot, now)
	if err != nil {
		return l.result(), err
	}
	epochChanged := l.state.Policy != s.Policy || l.state.BootID != s.BootID || l.state.UpdatedAt.IsZero()
	if epochChanged || now.Sub(l.lastSave) >= time.Duration(l.config.FlushSeconds)*time.Second {
		// 重命名后检查点才对外可见。此前写入失败时保留上一次已接受的状态，
		// 修复后可由 Close 保存。
		if err = writeTrafficState(l.config.StateFile, s); err != nil {
			var published *trafficDirectorySyncError
			if errors.As(err, &published) {
				// 读者已能看到替换后的文件。内存保留同一状态，
				// 在确认落盘前暂停采样。
				l.state = s
			}
			l.fatal = fmt.Errorf("traffic persistence stopped: %w", err)
			return l.result(), l.fatal
		}
		l.lastSave = now
	}
	l.state = s
	return r, nil
}

func (l *TrafficLedger) result() TrafficResult {
	raw, billing := trafficWindowResult(l.config, l.state, l.state.UpdatedAt)
	r := TrafficResult{Raw: raw, Billing: billing, Missing: []string{}}
	return r
}

func (l *TrafficLedger) Result() TrafficResult {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.result()
}

func (l *TrafficLedger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	err := l.save()
	unlockErr := l.lock.close()
	if err != nil {
		return err
	}
	return unlockErr
}

// 沿用原生时间窗口和重置日期计算。只合并同一个原生保存间隔内的采样，
// 本地午夜前后的采样分别保留，落盘时保持增量所属的原生重置周期。
func appendTrafficWindow(ns *NetStatic, name string, td TrafficData) {
	arr := ns.Interfaces[name]
	if len(arr) > 0 && sameTrafficBucket(arr[len(arr)-1].Timestamp, td.Timestamp) {
		p := &arr[len(arr)-1]
		p.Timestamp = td.Timestamp
		p.Rx += td.Rx
		p.Tx += td.Tx
		p.BillingRx += td.BillingRx
		p.BillingTx += td.BillingTx
	} else if td.Rx != 0 || td.Tx != 0 || td.BillingRx != 0 || td.BillingTx != 0 {
		arr = append(arr, td)
	}
	ns.Interfaces[name] = arr
}

func sameTrafficBucket(a, b uint64) bool {
	interval := uint64(DefaultSaveInterval)
	if interval == 0 || a/interval != b/interval {
		return false
	}
	ay, am, ad := time.Unix(int64(a), 0).In(time.Local).Date()
	by, bm, bd := time.Unix(int64(b), 0).In(time.Local).Date()
	return ay == by && am == bm && ad == bd
}

func purgeTrafficWindow(ns *NetStatic, now time.Time) {
	cutoff := uint64(now.Add(-time.Duration(DefaultDataPreserveDay * 24 * float64(time.Hour))).Unix())
	for name, arr := range ns.Interfaces {
		first := 0
		for first < len(arr) && arr[first].Timestamp < cutoff {
			first++
		}
		if first == len(arr) {
			delete(ns.Interfaces, name)
		} else {
			ns.Interfaces[name] = arr[first:]
		}
	}
}

func trafficWindowResult(c TrafficConfig, s TrafficState, now time.Time) (TrafficBytes, TrafficBytes) {
	if c.MonthRotate == 0 {
		return s.Raw, s.Billing
	}
	start := uint64(utils.GetLastResetDate(c.MonthRotate, now).Unix())
	window := totalTrafficBetween(s.Window.Interfaces, nil, start, uint64(now.Unix()))
	var raw, bill TrafficBytes
	for _, t := range window {
		raw.In += t.Rx
		raw.Out += t.Tx
		bill.In += t.BillingRx
		bill.Out += t.BillingTx
	}
	return raw, bill
}
