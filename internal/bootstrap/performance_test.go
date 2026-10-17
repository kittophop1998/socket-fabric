//go:build linux && integration && performance

package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"socketfabric/internal/config"
	"socketfabric/internal/testsupport/natsserver"
	sdk "socketfabric/sdk/go"
)

// The gateways and SDK load generator share this process; all process resource
// figures include both. NATS is a separate real process and is excluded.
type benchmarkCluster struct {
	servers []*Server
	urls    []string
	clients []*sdk.Client
	signer  *sdk.TokenSigner
}

func benchmarkInt(b *testing.B, name string, fallback, maximum int) int {
	b.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > maximum {
		b.Fatalf("%s must be between 1 and %d", name, maximum)
	}
	return value
}

func newBenchmarkCluster(b *testing.B, mode string) *benchmarkCluster {
	b.Helper()
	cluster := &benchmarkCluster{}
	cfg := testServer(b).config
	cfg.Auth.ClientTokenSecret = rand.Text() + rand.Text()
	cfg.Auth.PublisherKeys = map[string]string{"project-a": rand.Text() + rand.Text()}
	cfg.ShutdownTimeout = 3 * time.Second
	cfg.WebSocket.MaxConnections = 512
	cfg.WebSocket.AuthTimeout = 3 * time.Second
	cfg.WebSocket.WriteTimeout = time.Second
	cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, 5*time.Second
	nodes := 1
	if mode == "distributed" {
		broker := natsserver.Start(b, rand.Text()+rand.Text())
		cfg.Mode, nodes = mode, 2
		cfg.NATS = config.NATS{URL: broker.URL, Token: broker.Token, SubjectPrefix: "benchmark", ConnectTimeout: 3 * time.Second, PublishTimeout: 2 * time.Second}
	}
	var err error
	cluster.signer, err = sdk.NewTokenSigner([]byte(cfg.Auth.ClientTokenSecret))
	if err != nil {
		b.Fatal(err)
	}
	for range nodes {
		server, err := NewServer(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		if err != nil {
			b.Fatal(err)
		}
		url, cancel, result := startServer(b, server)
		b.Cleanup(func() {
			cancel()
			select {
			case err := <-result:
				if err != nil {
					b.Error("benchmark gateway cleanup", err)
				}
			case <-time.After(4 * time.Second):
				b.Error("benchmark gateway did not drain")
			}
		})
		waitHTTPStatus(b, url+"/readyz", http.StatusOK)
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConnsPerHost = 32
		transport.MaxConnsPerHost = 32
		b.Cleanup(transport.CloseIdleConnections)
		client, err := sdk.NewClient(sdk.ClientOptions{BaseURL: url, ProjectID: "project-a", PublisherKey: cfg.Auth.PublisherKeys["project-a"], HTTPClient: &http.Client{Transport: transport}, RequestTimeout: 3 * time.Second})
		if err != nil {
			b.Fatal(err)
		}
		cluster.servers = append(cluster.servers, server)
		cluster.urls = append(cluster.urls, url)
		cluster.clients = append(cluster.clients, client)
	}
	return cluster
}

func (c *benchmarkCluster) connect(ctx context.Context, node int) (*websocket.Conn, error) {
	token, err := c.signer.IssueToken(sdk.ClientTokenInput{ProjectID: "project-a", UserID: "benchmark-user", Rooms: []string{"room"}, TTL: 5 * time.Minute})
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, strings.Replace(c.urls[node], "http://", "ws://", 1)+"/ws", nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(128 << 10)
	for _, command := range []map[string]string{{"op": "auth", "token": token}, {"op": "join", "roomId": "room"}} {
		body, _ := json.Marshal(command)
		if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
			_ = conn.CloseNow()
			return nil, err
		}
		_, response, err := conn.Read(ctx)
		var frame struct{ Op string }
		if err != nil || json.Unmarshal(response, &frame) != nil || frame.Op != map[string]string{"auth": "authenticated", "join": "joined"}[command["op"]] {
			_ = conn.CloseNow()
			return nil, fmt.Errorf("benchmark authentication/subscription failed")
		}
	}
	return conn, nil
}

func benchmarkCPU() time.Duration {
	var usage syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
	return time.Duration(usage.Utime.Sec+usage.Stime.Sec)*time.Second + time.Duration(usage.Utime.Usec+usage.Stime.Usec)*time.Microsecond
}

func reportPercentiles(b *testing.B, samples []time.Duration, prefix string) {
	if len(samples) == 0 {
		return
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	for _, percentile := range []int{50, 95, 99} {
		index := min(len(samples)-1, (len(samples)*percentile+99)/100-1)
		b.ReportMetric(float64(samples[index].Nanoseconds())/1000, fmt.Sprintf("%s-p%d-us", prefix, percentile))
	}
}

func BenchmarkRealtimePublish(b *testing.B) {
	for _, mode := range []string{"single", "distributed"} {
		b.Run(mode, func(b *testing.B) {
			b.StopTimer()
			cluster := newBenchmarkCluster(b, mode)
			payload := strings.Repeat("x", benchmarkInt(b, "SF_BENCH_PAYLOAD_BYTES", 256, 32768))
			samples := make([]time.Duration, min(b.N, 65536))
			var next atomic.Uint64
			cpu := benchmarkCPU()
			started := time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					index := next.Add(1) - 1
					before := time.Now()
					_, err := cluster.clients[int(index)%len(cluster.clients)].Publish(context.Background(), sdk.PublishInput{RoomID: "room", Type: "benchmark.event", Data: payload})
					if err != nil {
						b.Error("benchmark publish rejected", err)
					}
					if int(index) < len(samples) {
						samples[index] = time.Since(before)
					}
				}
			})
			b.StopTimer()
			elapsed := time.Since(started)
			b.ReportMetric(float64(b.N)/elapsed.Seconds(), "accepted-events/s")
			b.ReportMetric(float64(benchmarkCPU()-cpu)/float64(elapsed)*100, "harness-CPU-percent")
			reportPercentiles(b, samples, "http")
		})
	}
}

type benchmarkPayload struct {
	Sequence int    `json:"sequence"`
	SentAt   int64  `json:"sentAt"`
	Padding  string `json:"padding"`
}

type benchmarkDelivery struct {
	id       string
	sequence int
	latency  time.Duration
	err      error
}

func BenchmarkRealtimeFanout(b *testing.B) { benchmarkFanout(b, false) }

func BenchmarkRealtimeFanoutWithSlowPeer(b *testing.B) { benchmarkFanout(b, true) }

func benchmarkFanout(b *testing.B, slow bool) {
	for _, mode := range []string{"single", "distributed"} {
		b.Run(mode, func(b *testing.B) {
			b.StopTimer()
			cluster := newBenchmarkCluster(b, mode)
			clients := benchmarkInt(b, "SF_BENCH_CLIENTS", 32, 256)
			payloadSize := benchmarkInt(b, "SF_BENCH_PAYLOAD_BYTES", 256, 32768)
			if slow {
				clients, payloadSize = min(clients, 8), 32768
			}
			ctx, cancel := context.WithCancel(context.Background())
			var readers sync.WaitGroup
			var sockets []*websocket.Conn
			b.Cleanup(func() {
				cancel()
				for _, conn := range sockets {
					_ = conn.CloseNow()
				}
				readers.Wait()
			})
			runtime.GC()
			var before, connected runtime.MemStats
			runtime.ReadMemStats(&before)
			beforeGoroutines := runtime.NumGoroutine()
			deliveries := make(chan benchmarkDelivery, clients)
			for i := range clients {
				connectCtx, done := context.WithTimeout(ctx, 3*time.Second)
				conn, err := cluster.connect(connectCtx, i%len(cluster.urls))
				done()
				if err != nil {
					b.Fatal(err)
				}
				sockets = append(sockets, conn)
				readers.Add(1)
				go func() {
					defer readers.Done()
					for {
						_, body, err := conn.Read(ctx)
						if err != nil {
							select {
							case deliveries <- benchmarkDelivery{err: err}:
							case <-ctx.Done():
							}
							return
						}
						var frame struct {
							Op    string
							Event sdk.Event
						}
						var payload benchmarkPayload
						if json.Unmarshal(body, &frame) != nil || frame.Op != "event" || json.Unmarshal(frame.Event.Data, &payload) != nil {
							select {
							case deliveries <- benchmarkDelivery{err: fmt.Errorf("unexpected benchmark frame")}:
							case <-ctx.Done():
							}
							return
						}
						select {
						case deliveries <- benchmarkDelivery{id: frame.Event.EventID, sequence: payload.Sequence, latency: time.Duration(time.Now().UnixNano() - payload.SentAt)}:
						case <-ctx.Done():
							return
						}
					}
				}()
			}
			if slow {
				conn, err := cluster.connect(ctx, 0)
				if err != nil {
					b.Fatal(err)
				}
				sockets = append(sockets, conn) // Deliberately never read again.
			}
			runtime.GC()
			runtime.ReadMemStats(&connected)
			heapPerConnection := float64(max(connected.HeapAlloc, before.HeapAlloc)-before.HeapAlloc) / float64(len(sockets))
			goroutinesPerConnection := float64(runtime.NumGoroutine()-beforeGoroutines) / float64(len(sockets))
			samples := make([]time.Duration, 0, min(b.N*clients, 65536))
			httpSamples := make([]time.Duration, 0, min(b.N, 65536))
			padding := strings.Repeat("x", payloadSize)
			cpu, started := benchmarkCPU(), time.Now()
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			for sequence := range b.N {
				begin := time.Now()
				result, err := cluster.clients[0].Publish(ctx, sdk.PublishInput{RoomID: "room", Type: "benchmark.event", Data: benchmarkPayload{Sequence: sequence, SentAt: begin.UnixNano(), Padding: padding}})
				if err != nil {
					b.Fatal(err)
				}
				if len(httpSamples) < cap(httpSamples) {
					httpSamples = append(httpSamples, time.Since(begin))
				}
				deadline := time.NewTimer(3 * time.Second)
				for range clients {
					select {
					case delivery := <-deliveries:
						if delivery.err != nil || delivery.sequence != sequence || delivery.id != result.Event.EventID {
							b.Fatal("missing, duplicate, out-of-order or rejected benchmark delivery")
						}
						if len(samples) < cap(samples) {
							samples = append(samples, delivery.latency)
						}
					case <-deadline.C:
						b.Fatal("benchmark client delivery deadline")
					}
				}
				deadline.Stop()
			}
			b.StopTimer()
			elapsed := time.Since(started)
			b.ReportMetric(float64(b.N)/elapsed.Seconds(), "logical-events/s")
			b.ReportMetric(float64(b.N*clients)/elapsed.Seconds(), "deliveries/s")
			b.ReportMetric(heapPerConnection, "harness-heap-B/conn")
			b.ReportMetric(goroutinesPerConnection, "harness-goroutines/conn")
			b.ReportMetric(float64(benchmarkCPU()-cpu)/float64(elapsed)*100, "harness-CPU-percent")
			if slow {
				remaining := 0
				for _, server := range cluster.servers {
					remaining += server.gateway.Snapshot()["authenticatedConnections"].(int)
				}
				b.ReportMetric(float64(clients+1-remaining), "slow-peer-disconnected")
			}
			reportPercentiles(b, samples, "e2e")
			reportPercentiles(b, httpSamples, "http")
		})
	}
}

func BenchmarkRealtimeConnectionLifecycle(b *testing.B) {
	for _, mode := range []string{"single", "distributed"} {
		b.Run(mode, func(b *testing.B) {
			b.StopTimer()
			cluster := newBenchmarkCluster(b, mode)
			samples := make([]time.Duration, min(b.N, 65536))
			var next atomic.Uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.StartTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					index, begin := next.Add(1)-1, time.Now()
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					conn, err := cluster.connect(ctx, int(index)%len(cluster.urls))
					cancel()
					if err != nil {
						b.Error(err)
						continue
					}
					if int(index) < len(samples) {
						samples[index] = time.Since(begin)
					}
					_ = conn.CloseNow()
				}
			})
			b.StopTimer()
			reportPercentiles(b, samples, "connect-auth-join")
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "connection-lifecycles/s")
		})
	}
}
