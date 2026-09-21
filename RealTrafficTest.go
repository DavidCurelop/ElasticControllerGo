package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// TrafficPhase defines an open-loop phase calibrated for AWS CloudWatch intervals
// and EC2 launch/cooldown life cycles.
type TrafficPhase struct {
	Name     string
	Duration time.Duration
	RPS      int
}

// WindowStats tracks metrics over a 1-second reporting window.
type WindowStats struct {
	mu          sync.Mutex
	status2xx   int64
	status5xx   int64
	networkErrs int64
	latenciesMs []float64
}

func (w *WindowStats) record(statusCode int, err error, latency time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.latenciesMs = append(w.latenciesMs, float64(latency.Microseconds())/1000.0)
	if err != nil {
		w.networkErrs++
		return
	}
	if statusCode >= 200 && statusCode < 300 {
		w.status2xx++
	} else if statusCode >= 500 {
		w.status5xx++
	} else {
		w.networkErrs++
	}
}

func (w *WindowStats) flush() (s2xx, s5xx, errs int64, p50, p90, p99 float64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	s2xx = w.status2xx
	s5xx = w.status5xx
	errs = w.networkErrs
	count := len(w.latenciesMs)

	if count > 0 {
		sort.Float64s(w.latenciesMs)
		p50 = w.latenciesMs[int(float64(count)*0.50)]
		p90 = w.latenciesMs[int(float64(count)*0.90)]
		p99 = w.latenciesMs[int(float64(count)*0.99)]
	}

	// Reset window
	w.status2xx = 0
	w.status5xx = 0
	w.networkErrs = 0
	w.latenciesMs = w.latenciesMs[:0]
	return
}

func main() {
	targetURL := flag.String("url", "http://controllerelb-344953633.us-east-1.elb.amazonaws.com", "Target ELB endpoint")
	outputFile := flag.String("out", "experiment_metrics.csv", "Output CSV file for time-series metrics")
	flag.Parse()

	// Phase lengths must exceed CloudWatch metric lag (1-min) and EC2 bootstrap/health check time (2-4 min)
	// to properly test horizontal scaling, cooldowns, and stabilization without flapping.
	phases := []TrafficPhase{
		{"1. Baseline / Warm-up", 8 * time.Minute, 100},    // 1-instance baseline (Min: 1)
		{"2. Sudden Demand Spike", 10 * time.Minute, 1500}, // Overload: triggers INCREASE_CAPACITY up to max 5
		{"3. Sustained High", 8 * time.Minute, 500},       // Sustained: evaluates stability and anti-oscillation
		{"4. Traffic Cooldown", 10 * time.Minute, 250},     // Low demand: triggers REDUCE_CAPACITY back to base
	}

	// Prepare CSV logger for Deliverable 10.3 (Experimental Evidence)
	csvFile, err := os.Create(*outputFile)
	if err != nil {
		fmt.Printf("Failed to create CSV file: %v\n", err)
		return
	}
	defer csvFile.Close()

	writer := csv.NewWriter(csvFile)
	defer writer.Flush()

	header := []string{
		"Timestamp", "Phase", "TargetRPS", "ActiveConns",
		"HTTP_2xx", "HTTP_5xx", "Errors", "P50_Latency_ms", "P90_Latency_ms", "P99_Latency_ms",
	}
	_ = writer.Write(header)
	writer.Flush()

	// High-throughput HTTP Transport optimized for persistent connection reuse
	transport := &http.Transport{
		MaxIdleConns:        10000,
		MaxIdleConnsPerHost: 10000,
		MaxConnsPerHost:     10000,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   8 * time.Second, // Realistic user SLA threshold
	}

	var activeConns int64
	stats := &WindowStats{latenciesMs: make([]float64, 0, 1000)}

	// Setup graceful cancellation on SIGINT/SIGTERM
	rootCtx, rootCancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer rootCancel()

	// Background reporter logging metrics to CSV and console every second
	currentPhaseName := "Initializing"
	currentTargetRPS := 0
	metricsTicker := time.NewTicker(1 * time.Second)
	defer metricsTicker.Stop()

	go func() {
		for {
			select {
			case <-rootCtx.Done():
				return
			case t := <-metricsTicker.C:
				s2xx, s5xx, errs, p50, p90, p99 := stats.flush()
				conns := atomic.LoadInt64(&activeConns)

				record := []string{
					t.UTC().Format(time.RFC3339),
					currentPhaseName,
					strconv.Itoa(currentTargetRPS),
					strconv.FormatInt(conns, 10),
					strconv.FormatInt(s2xx, 10),
					strconv.FormatInt(s5xx, 10),
					strconv.FormatInt(errs, 10),
					fmt.Sprintf("%.2f", p50),
					fmt.Sprintf("%.2f", p90),
					fmt.Sprintf("%.2f", p99),
				}
				_ = writer.Write(record)
				writer.Flush()

				// Status line every second
				fmt.Printf("[%s] Active: %4d | 2xx: %4d/s | 5xx: %3d/s | Err: %3d/s | Latency (p50: %5.1fms, p90: %6.1fms, p99: %6.1fms)\n",
					t.Format("15:04:05"), conns, s2xx, s5xx, errs, p50, p90, p99)
			}
		}
	}()

	fmt.Printf("Starting experiment against: %s\n", *targetURL)
	fmt.Printf("Logging continuous metrics to: %s\n", *outputFile)

	for i, phase := range phases {
		select {
		case <-rootCtx.Done():
			fmt.Println("\nExperiment interrupted by user.")
			return
		default:
		}

		currentPhaseName = phase.Name
		currentTargetRPS = phase.RPS

		fmt.Printf("\n=== [Phase %d/%d] %s (Target RPS: %d, Duration: %v) ===\n",
			i+1, len(phases), phase.Name, phase.RPS, phase.Duration)

		phaseCtx, phaseCancel := context.WithTimeout(rootCtx, phase.Duration)

		interval := time.Second / time.Duration(phase.RPS)
		ticker := time.NewTicker(interval)

		// Dispatcher Goroutine
		go func() {
			for {
				select {
				case <-phaseCtx.Done():
					ticker.Stop()
					return
				case <-ticker.C:
					atomic.AddInt64(&activeConns, 1)

					go func() {
						defer atomic.AddInt64(&activeConns, -1)

						start := time.Now()
						req, err := http.NewRequestWithContext(phaseCtx, http.MethodGet, *targetURL, nil)
						if err != nil {
							stats.record(0, err, time.Since(start))
							return
						}

						resp, err := client.Do(req)
						duration := time.Since(start)

						if err != nil {
							stats.record(0, err, duration)
							return
						}

						// Critical: drain response body completely so TCP connections can be reused
						_, _ = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()

						stats.record(resp.StatusCode, nil, duration)
					}()
				}
			}
		}()

		// Wait for phase duration to complete
		<-phaseCtx.Done()
		phaseCancel() // Clean up context timer immediately (no deferred context leak)

		// Allow in-flight requests to settle before next phase transition
		time.Sleep(3 * time.Second)
	}

	fmt.Println("\nTraffic simulation complete. Metrics written to CSV.")
}
