package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/HdrHistogram/hdrhistogram-go"
	"golang.org/x/time/rate"
)

// RequestFunc is a function type for sending requests (currently unused)
type RequestFunc func() error

// Benchmark holds benchmark parameters.
type Benchmark struct {
	Threads      int               // Number of threads to use.
	Connections  int               // Number of connections to keep open.
	Rate         float64           // Target request rate in requests/sec.
	Duration     time.Duration     // Duration of the benchmark test.
	Timeout      time.Duration     // Timeout for HTTP requests.
	Method       string            // HTTP method (GET, POST, etc.).
	Headers      map[string]string // HTTP headers to include in requests.
	Body         string            // Request body.
	URL          string            // Target URL for benchmarking.
	PrintLatency bool              // Whether to print detailed latency statistics.
}

// BenchResult holds the benchmark results.
type BenchResult struct {
	Throughput  float64                 // Requests per second.
	Counter     int                     // Number of successful requests.
	Errors      int                     // Number of errors (HTTP errors + other errors).
	Omitted     int                     // Number of requests omitted due to rate limiting.
	Latency     *hdrhistogram.Histogram // Latency histogram.
	TotalTime   time.Duration           // Total benchmark execution time.
	Threads     int                     // Number of threads used.
	Connections int                     // Number of connections used.
}

// runBenchmarkCmd executes the benchmark based on command-line parameters.
func runBenchmarkCmd(params Benchmark) {
	fmt.Printf("Running %s test @ %s\n", params.Duration, params.URL)
	fmt.Printf("  %d threads and %d connections\n", params.Threads, params.Connections)
	result := params.Run()
	PrintBenchResult(result, params) // Pass params to PrintBenchResult
}

// arrayFlags is a custom type to handle multiple header flags.
type arrayFlags []string

// Set appends a flag value to the arrayFlags.
func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

// String returns a string representation of arrayFlags.
// This is required to satisfy the flag.Value interface.
func (i *arrayFlags) String() string {
	return "" // Return empty string as the default value representation is not needed.
}

// parseHeaders parses header strings into a map[string]string.
func parseHeaders(headers []string) map[string]string {
	headerMap := make(map[string]string)
	for _, header := range headers {
		parts := strings.SplitN(header, ":", 2)
		if len(parts) != 2 {
			fmt.Println("Warning: invalid header format:", header)
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		headerMap[key] = value
	}
	return headerMap
}

// localResult holds results for each goroutine.
type localResult struct {
	errors  int                     // Number of errors.
	counter int                     // Number of successful requests.
	latency *hdrhistogram.Histogram // Latency histogram.
}

// executioner manages the benchmark execution.
type executioner struct {
	eventsGenerator                  // Event generator.
	benchmark       Benchmark        // Benchmark parameters.
	results         chan localResult // Channel to collect results from goroutines.
	startTime       time.Time        // Benchmark start time.
}

// eventsGenerator generates events for request execution.
type eventsGenerator struct {
	lock      sync.Mutex         // Mutex for protecting omitted count.
	eventsBuf chan time.Time     // Channel to buffer events.
	doneCtx   context.Context    // Context for signaling completion.
	cancel    context.CancelFunc // Cancel function for the context.
	omitted   int                // Number of skipped events due to rate limiting.
}

// Run executes the benchmark and returns the results.
func (b Benchmark) Run() BenchResult {
	execution := b.newExecution()
	execution.generateEvents(b.Rate, 2*b.Connections*b.Threads) // Burst size adjusted for Coordinated Omission
	for i := 0; i < b.Connections*b.Threads; i++ {              // Goroutine count adjusted for Coordinated Omission
		go execution.sendRequests()
	}

	execution.awaitDone()

	return execution.summarizeResults()
}

// newExecution creates a new executioner instance.
func (b Benchmark) newExecution() *executioner {
	return &executioner{
		eventsGenerator: newEventsGenerator(b.Duration, int(b.Rate*10) /*10 seconds buffer*/),
		benchmark:       b,
		results:         make(chan localResult, b.Connections*b.Threads), // Channel buffer size adjusted for Coordinated Omission
		startTime:       time.Now(),
	}
}

// newEventsGenerator creates a new eventsGenerator instance.
func newEventsGenerator(duration time.Duration, bufSize int) eventsGenerator {
	doneCtx, cancel := context.WithTimeout(context.Background(), duration)
	return eventsGenerator{
		lock:      sync.Mutex{},
		doneCtx:   doneCtx,
		cancel:    cancel,
		eventsBuf: make(chan time.Time, bufSize),
	}
}

// generateEvents generates events at the specified throughput and sends them to the eventsBuf channel.
func (e *eventsGenerator) generateEvents(throughput float64, burstSize int) {
	go func() {
		omitted := 0
		rateLimiter := rate.NewLimiter(rate.Limit(throughput), burstSize)
		for err := rateLimiter.Wait(e.doneCtx); err == nil; err = rateLimiter.Wait(e.doneCtx) {
			select {
			case e.eventsBuf <- time.Now(): // Send event time to the channel
			default:
				omitted++ // Skip event if channel is blocked (rate limited)
			}
		}

		close(e.eventsBuf) // Signal event generation completion
		e.lock.Lock()
		e.omitted = omitted
		e.lock.Unlock()
	}()
}

// omittedCount returns the number of skipped events.
func (e *eventsGenerator) omittedCount() int {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.omitted
}

// awaitDone waits for the completion context to be done.
func (e *eventsGenerator) awaitDone() {
	<-e.doneCtx.Done()
	e.cancel() // Cancel the context
}

// sendRequests is the goroutine that sends HTTP requests.
func (e *executioner) sendRequests() {
	res := localResult{
		errors:  0,
		counter: 0,
		latency: createHistogram(),
	}

	client := &http.Client{Timeout: e.benchmark.Timeout}                  // Apply timeout setting
	req, err := http.NewRequest(e.benchmark.Method, e.benchmark.URL, nil) // Create HTTP request
	if err != nil {
		fmt.Println("Error creating request:", err) // Request creation error
		res.errors++                                // Count request creation error as an error
		e.results <- res
		return
	}

	if e.benchmark.Body != "" {
		req.Body = io.NopCloser(strings.NewReader(e.benchmark.Body)) // Set request body
	}

	for key, value := range e.benchmark.Headers {
		req.Header.Set(key, value) // Set headers
	}

	done := false
	for !done {
		select {
		case <-e.doneCtx.Done(): // Completion signal received
			done = true
		case _, ok := <-e.eventsBuf: // Event received from the event channel
			if ok {
				res.counter++
				startTime := time.Now()          // Record request start time
				resp, err := client.Do(req)      // Send HTTP request
				elapsed := time.Since(startTime) // Measure request processing time

				if err != nil {
					res.errors++
					log.Printf("HTTP request error: %v, URL: %s\n", err, e.benchmark.URL) // Log HTTP request error (including URL)
				} else {
					if resp != nil {
						defer resp.Body.Close()
						_, err := io.Copy(io.Discard, resp.Body) // Discard response body
						if err != nil {
							log.Printf("Error reading response body: %v, URL: %s\n", err, e.benchmark.URL) // Log response body read error
							res.errors++                                                                   // Count response body read error as an error
						}
						if resp.StatusCode >= 400 {
							res.errors++                                                                          // Count HTTP status code >= 400 as an error
							log.Printf("HTTP error status code: %d, URL: %s\n", resp.StatusCode, e.benchmark.URL) // Log HTTP error (status code and URL)
						}
					} else {
						res.errors++                                                   // Count nil response as an error
						log.Printf("HTTP response is nil, URL: %s\n", e.benchmark.URL) // Log nil response error
					}
				}

				if recordErr := res.latency.RecordValue(int64(elapsed)); recordErr != nil { // Record latency
					log.Printf("failed to record latency: %v\n", recordErr) // Log latency recording error
				}
			} else {
				done = true // Exit if event channel is closed
			}
		}
	}

	e.results <- res // Send local results to the results channel
}

// summarizeResults aggregates results from all goroutines and returns BenchResult.
func (e *executioner) summarizeResults() BenchResult {
	counter := 0
	errors := 0
	latency := createHistogram()

	for i := 0; i < e.benchmark.Connections*e.benchmark.Threads; i++ { // Collect results from all goroutines
		localRes := <-e.results
		counter += localRes.counter
		errors += localRes.errors
		latency.Merge(localRes.latency)
	}

	totalTime := time.Since(e.startTime)

	return BenchResult{
		Throughput:  float64(counter) / totalTime.Seconds(),
		Counter:     counter,
		Errors:      errors,
		Omitted:     e.omittedCount(),
		Latency:     latency,
		TotalTime:   totalTime,
		Threads:     e.benchmark.Threads,     // Include thread count in results
		Connections: e.benchmark.Connections, // Include connection count in results
	}
}

// createHistogram creates a new histogram for latency measurement.
func createHistogram() *hdrhistogram.Histogram {
	return hdrhistogram.New(0, int64(time.Minute), 3) // Range from 0ns to 1 minute, 3 digits precision
}

// PrintBenchResult formats and prints the benchmark results.
func PrintBenchResult(result BenchResult, params Benchmark) { // params added as argument
	fmt.Printf("\n")
	fmt.Printf("Running %s test @ %s\n", params.Duration, params.URL)
	fmt.Printf("  %d threads and %d connections\n", params.Threads, params.Connections)

	if result.Errors > 0 || result.Omitted > 0 {
		fmt.Printf("  Socket errors: %d reads, %d writes, %d connects, %d timeouts\n", 0, 0, 0, result.Errors) // Socket error types not distinguished yet (improvement needed)
		if result.Omitted > 0 {
			fmt.Printf("  Non-2xx or 3xx responses: %d\n", result.Errors) // HTTP errors and other errors not distinguished yet (improvement needed)
			fmt.Printf("  Omitted requests: %d\n", result.Omitted)
		}

	}

	fmt.Println()
	fmt.Println("Thread Stats   Avg      Stdev     Max   +/- Stdev")

	// Calculate average latency, standard deviation, and max latency.
	avgLatency := time.Duration(result.Latency.Mean())
	stdDevLatency := time.Duration(result.Latency.StdDev())
	maxLatency := time.Duration(result.Latency.Max())

	// Calculate +/- Stdev.
	plusMinusStdev := float64(stdDevLatency) / float64(avgLatency) * 100.0

	fmt.Printf("  Latency   %8s %8s %8s %6.2f%%\n",
		avgLatency,
		stdDevLatency,
		maxLatency,
		plusMinusStdev,
	)
	fmt.Printf("  Req/Sec    %8.2f %8.2f %8.2f %6.2f%%\n", // Mimic wrk2 output format (Stdev, Max of Req/Sec are not implemented, showing average)
		result.Throughput/float64(params.Threads), // Average Req/Sec per thread (approximate)
		0.0, // Stdev (not implemented)
		result.Throughput/float64(params.Threads), // Max (not implemented, showing average)
		0.0, // +/- Stdev (not implemented)
	)
	fmt.Println()
	fmt.Println("Latency Distribution:")
	printHistogram(result.Latency)

	fmt.Printf("\n")
	fmt.Printf("  Requests/sec: %10.2f\n", result.Throughput)
	fmt.Printf("  Transfer/sec: %10.2fMB\n", 0.0) // Mimic wrk2 output format (not implemented, showing 0.0MB)
}

// printHistogram prints the histogram in a specified format.
func printHistogram(hist *hdrhistogram.Histogram) {

	fmt.Println("    50%    ", time.Duration(hist.ValueAtQuantile(50.0)))
	fmt.Println("    75%    ", time.Duration(hist.ValueAtQuantile(75.0)))
	fmt.Println("    90%    ", time.Duration(hist.ValueAtQuantile(90.0)))
	fmt.Println("    99%    ", time.Duration(hist.ValueAtQuantile(99.0)))
	fmt.Println("   99.9%    ", time.Duration(hist.ValueAtQuantile(99.9)))
	fmt.Println("  99.99%    ", time.Duration(hist.ValueAtQuantile(99.99)))
	fmt.Println("   100%    ", time.Duration(hist.ValueAtQuantile(100.0)))
}

func main() {
	var connections int
	var threads int
	var rateLimit float64
	var duration time.Duration
	var timeout time.Duration
	var method string
	var headers arrayFlags
	var body string
	var printLatency bool
	var versionFlag bool

	flag.IntVar(&connections, "c", 10, "Connections to keep open")
	flag.IntVar(&connections, "connections", 10, "Connections to keep open")

	flag.IntVar(&threads, "t", 2, "Number of threads to use")
	flag.IntVar(&threads, "threads", 2, "Number of threads to use")

	flag.Float64Var(&rateLimit, "r", 10000, "Target request rate in requests/sec")
	flag.Float64Var(&rateLimit, "rate", 10000, "Target request rate in requests/sec")

	flag.DurationVar(&duration, "d", 10*time.Second, "Duration of test")
	flag.DurationVar(&duration, "duration", 10*time.Second, "Duration of test")

	flag.DurationVar(&timeout, "T", 0*time.Second, "Socket/request timeout")
	flag.DurationVar(&timeout, "timeout", 0*time.Second, "Socket/request timeout")

	flag.StringVar(&method, "m", "GET", "HTTP method")
	flag.StringVar(&method, "method", "GET", "HTTP method")

	flag.Var(&headers, "H", "Add header to request")
	flag.Var(&headers, "header", "Add header to request")

	flag.StringVar(&body, "b", "", "Request body")
	flag.StringVar(&body, "body", "", "Request body")

	flag.BoolVar(&printLatency, "latency", false, "Print latency statistics")

	flag.BoolVar(&versionFlag, "v", false, "Show version and exit")
	flag.BoolVar(&versionFlag, "version", false, "Show version and exit")

	flag.Parse()

	if versionFlag {
		fmt.Println("wrk3 modified by Okamyuji (Go version)") // Display version information
		os.Exit(0)
	}

	if len(flag.Args()) < 1 {
		fmt.Println("Usage: wrk3 [options] <url>") // Display usage information
		flag.PrintDefaults()
		os.Exit(1)
	}

	url := flag.Args()[0]
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		fmt.Println("Error: URL must start with http:// or https://") // Corrected error message
		os.Exit(1)
	}

	// Input value validation
	if connections <= 0 {
		fmt.Println("Error: connections must be greater than 0")
		os.Exit(1)
	}
	if threads <= 0 {
		fmt.Println("Error: threads must be greater than 0")
		os.Exit(1)
	}
	if rateLimit < 0 {
		fmt.Println("Error: rate must be non-negative")
		os.Exit(1)
	}

	benchParams := Benchmark{
		Threads:      threads,
		Connections:  connections,
		Rate:         rateLimit,
		Duration:     duration,
		Timeout:      timeout,
		Method:       method,
		Headers:      parseHeaders(headers),
		Body:         body,
		URL:          url,          // Set URL to Benchmark struct
		PrintLatency: printLatency, // Set latency detail print flag
	}

	runBenchmarkCmd(benchParams) // Call benchmark execution function
}
