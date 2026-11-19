package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	v2 "github.com/komari-monitor/komari-agent/protocol/v2"
	"github.com/komari-monitor/komari-agent/ws"
)

type pingWireResult struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		TaskID     uint   `json:"task_id"`
		PingType   string `json:"ping_type"`
		Value      int    `json:"value"`
		FinishedAt string `json:"finished_at"`
	} `json:"params"`
}

func decodePingWire(t *testing.T, body []byte) pingWireResult {
	t.Helper()
	var result pingWireResult
	if err := json.Unmarshal(body, &result); err != nil {
		t.Errorf("decode ping result: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Errorf("decode envelope: %v", err)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(envelope["params"], &params); err != nil {
		t.Errorf("decode params: %v", err)
	}
	if len(envelope) != 3 || len(params) != 4 || result.JSONRPC != v2.Version || result.Method != v2.MethodAgentPingResult {
		t.Errorf("native ping result shape changed: %s", body)
	}
	if _, err := time.Parse(time.RFC3339Nano, result.Params.FinishedAt); err != nil {
		t.Errorf("finished_at: %v", err)
	}
	return result
}

func readPingPOST(t *testing.T, r *http.Request) pingWireResult {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != "/api/clients/v2/rpc" || r.URL.Query().Get("token") != "agent-token" {
		t.Errorf("unexpected native POST route: %s %s", r.Method, r.URL.Path)
	}
	var reader io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip: %v", err)
			return pingWireResult{}
		}
		defer gz.Close()
		reader = gz
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Errorf("read: %v", err)
	}
	return decodePingWire(t, body)
}

func TestPingTaskPreservesSingleMeasurement(t *testing.T) {
	for _, protocol := range []string{"icmp", "tcp", "http"} {
		for _, tc := range []struct {
			name    string
			latency int64
			err     error
			want    int
		}{
			{"1200ms_success", 1200, nil, 1200},
			{"near_timeout_success", 2999, nil, 2999},
			{"zero_is_success", 0, nil, 0},
			{"timeout", -1, context.DeadlineExceeded, -1},
			{"execution_error", -1, errors.New("probe execution failed"), -1},
			{"error_with_latency", 1200, errors.New("http status not ok"), -1},
			{"cancelled_measurement", -1, context.Canceled, -1},
		} {
			t.Run(protocol+"/"+tc.name, func(t *testing.T) {
				uploads := make(chan pingWireResult, 8)
				panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					uploads <- readPingPOST(t, r)
					w.WriteHeader(http.StatusOK)
				}))
				defer panel.Close()
				preserveAgentConfig(t, panel.URL)
				calls := 0
				start := time.Now()
				newPingTask(nil, 42, protocol, "test-target", func(kind, target string, timeout time.Duration) (int64, error) {
					calls++
					if kind != protocol || target != "test-target" || timeout != 3*time.Second {
						t.Errorf("measurement parameters: %s %s %s", kind, target, timeout)
					}
					if calls > 1 {
						return 1, nil
					} // A later faster sample must never be requested.
					return tc.latency, tc.err
				})
				end := time.Now()
				if calls != 1 || len(uploads) != 1 {
					t.Fatalf("measurements=%d, results=%d", calls, len(uploads))
				}
				got := (<-uploads).Params
				if got.TaskID != 42 || got.PingType != protocol || got.Value != tc.want {
					t.Fatalf("result=%+v, want value %d", got, tc.want)
				}
				finished, _ := time.Parse(time.RFC3339Nano, got.FinishedAt)
				if finished.Before(start) || finished.After(end) {
					t.Fatalf("completion timestamp outside measurement round: %s", finished)
				}
			})
		}
	}
}

func TestPingTaskInvalidIDDoesNotMeasureOrReport(t *testing.T) {
	var uploads atomic.Int32
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { uploads.Add(1) }))
	defer panel.Close()
	preserveAgentConfig(t, panel.URL)
	calls := 0
	newPingTask(nil, 0, "icmp", "unused", func(string, string, time.Duration) (int64, error) { calls++; return 0, nil })
	if calls != 0 || uploads.Load() != 0 {
		t.Fatal("invalid task measured or reported")
	}
}

func TestPingTaskPOSTFailureDoesNotRepeatMeasurementOrUpload(t *testing.T) {
	for _, compression := range []bool{false, true} {
		t.Run(map[bool]string{false: "gzip", true: "plain"}[compression], func(t *testing.T) {
			var uploads atomic.Int32
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				uploads.Add(1)
				readPingPOST(t, r)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer panel.Close()
			preserveAgentConfig(t, panel.URL)
			flags.DisableCompression = compression
			calls := 0
			newPingTask(nil, 7, "tcp", "unused", func(string, string, time.Duration) (int64, error) { calls++; return 1200, nil })
			if calls != 1 || uploads.Load() != 1 {
				t.Fatalf("POST failure caused repeat: measurements=%d uploads=%d", calls, uploads.Load())
			}
		})
	}
}

func TestPingTaskWebSocketReportsOnceWithoutPOSTReplay(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "connected", true: "closed"}[closed], func(t *testing.T) {
			var posts atomic.Int32
			received := make(chan pingWireResult, 4)
			done := make(chan struct{})
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/ws" {
					posts.Add(1)
					w.WriteHeader(http.StatusOK)
					return
				}
				c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					close(done)
					return
				}
				defer c.Close()
				defer close(done)
				for {
					_, body, err := c.ReadMessage()
					if err != nil {
						return
					}
					received <- decodePingWire(t, body)
				}
			}))
			defer panel.Close()
			preserveAgentConfig(t, panel.URL)
			c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(panel.URL, "http")+"/ws", nil)
			if err != nil {
				t.Fatal(err)
			}
			safe := ws.NewSafeConn(c)
			defer safe.Close()
			if closed {
				safe.Close()
			}
			calls := 0
			newPingTask(safe, 8, "tcp", "unused", func(string, string, time.Duration) (int64, error) { calls++; return 1200, nil })
			safe.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("WS reader did not stop")
			}
			if calls != 1 || posts.Load() != 0 {
				t.Fatalf("measurements=%d POST replays=%d", calls, posts.Load())
			}
			want := 1
			if closed {
				want = 0
			}
			if len(received) != want {
				t.Fatalf("WS results=%d want=%d", len(received), want)
			}
			if want == 1 {
				got := <-received
				if got.Params.Value != 1200 || got.Params.PingType != "tcp" || got.Params.TaskID != 8 {
					t.Fatal(got)
				}
			}
		})
	}
}

func TestNewPingTaskUsesNativeHTTPAndUnsupportedType(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var probes atomic.Int32
			uploads := make(chan pingWireResult, 8)
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/probe" {
					probes.Add(1)
					w.WriteHeader(status)
					return
				}
				uploads <- readPingPOST(t, r)
				w.WriteHeader(http.StatusOK)
			}))
			defer panel.Close()
			preserveAgentConfig(t, panel.URL)
			NewPingTask(nil, 9, "http", panel.URL+"/probe")
			if probes.Load() != 1 || len(uploads) != 1 {
				t.Fatalf("probes=%d uploads=%d", probes.Load(), len(uploads))
			}
			result := <-uploads
			if status == http.StatusNoContent && result.Params.Value < 0 {
				t.Fatal("successful HTTP rejected")
			}
			if status == http.StatusServiceUnavailable && result.Params.Value != -1 {
				t.Fatal("HTTP status error accepted")
			}
			NewPingTask(nil, 10, "unsupported", "must-not-resolve.invalid")
			if probes.Load() != 1 || len(uploads) != 1 || (<-uploads).Params.Value != -1 {
				t.Fatal("unsupported protocol behavior changed")
			}
		})
	}
}

func TestPingTaskConcurrentRoundsKeepIndependentResults(t *testing.T) {
	var mu sync.Mutex
	got := map[uint]int{}
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result := readPingPOST(t, r)
		mu.Lock()
		if _, ok := got[result.Params.TaskID]; ok {
			t.Errorf("duplicate result for task %d", result.Params.TaskID)
		}
		got[result.Params.TaskID] = result.Params.Value
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer panel.Close()
	preserveAgentConfig(t, panel.URL)
	const rounds = 24
	var wg sync.WaitGroup
	var measurements atomic.Int32
	for i := 1; i <= rounds; i++ {
		wg.Add(1)
		go func(id uint) {
			defer wg.Done()
			newPingTask(nil, id, "tcp", "unused", func(string, string, time.Duration) (int64, error) { measurements.Add(1); return int64(1200 + id), nil })
		}(uint(i))
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if measurements.Load() != rounds || len(got) != rounds {
		t.Fatalf("measurements=%d results=%d", measurements.Load(), len(got))
	}
	for i := 1; i <= rounds; i++ {
		if got[uint(i)] != 1200+i {
			t.Fatalf("round %d result %d", i, got[uint(i)])
		}
	}
}

func TestPingFallbackCancellationDoesNotDispatchOrRetry(t *testing.T) {
	var pulls atomic.Int32
	var results atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			defer gz.Close()
			reader = gz
		}
		var req v2.Request
		if err := json.NewDecoder(reader).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Method == v2.MethodAgentPull {
			pulls.Add(1)
			once.Do(func() { close(started) })
			<-release
			return
		}
		if req.Method == v2.MethodAgentPingResult {
			results.Add(1)
		}
	}))
	defer panel.Close()
	defer close(release)
	preserveAgentConfig(t, panel.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runV2PullLoop(ctx, nil)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("pull did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled pull did not stop")
	}
	if pulls.Load() != 1 || results.Load() != 0 {
		t.Fatalf("cancelled fallback repeated/dispatched: pulls=%d results=%d", pulls.Load(), results.Load())
	}
}

func TestPingFallbackEventDeduplicationKeepsSingleDispatch(t *testing.T) {
	eventID := t.Name()
	defer func() { v2AckMu.Lock(); delete(v2SeenEvents, eventID); v2AckMu.Unlock() }()
	var dispatched atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if markV2EventSeen(eventID) {
				dispatched.Add(1)
			}
		}()
	}
	wg.Wait()
	if dispatched.Load() != 1 {
		t.Fatalf("duplicate event permitted %d dispatches", dispatched.Load())
	}
}

func TestNewPingTaskMakesOneNativeTCPConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections atomic.Int32
	accepted := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			accepted <- struct{}{}
			conn.Close()
		}
	}()
	defer func() { listener.Close(); <-done }()
	results := make(chan pingWireResult, 4)
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		results <- readPingPOST(t, r)
		w.WriteHeader(http.StatusOK)
	}))
	defer panel.Close()
	preserveAgentConfig(t, panel.URL)
	NewPingTask(nil, 56, "tcp", listener.Addr().String())
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("TCP connection not accepted")
	}
	listener.Close()
	<-done
	if connections.Load() != 1 || len(results) != 1 {
		t.Fatalf("TCP connections=%d results=%d", connections.Load(), len(results))
	}
	result := <-results
	if result.Params.TaskID != 56 || result.Params.PingType != "tcp" || result.Params.Value < 0 {
		t.Fatal(result)
	}
}
