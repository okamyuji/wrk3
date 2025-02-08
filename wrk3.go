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

// RequestFunc is a function that returns an error
type RequestFunc func() error

// Benchmark is a struct that contains the benchmark parameters
type Benchmark struct {
	Threads      int
	Connections  int
	Rate         float64
	Duration     time.Duration
	Timeout      time.Duration
	Method       string
	Headers      map[string]string
	Body         string
	URL          string
	SendRequest  RequestFunc
	PrintLatency bool
}

// BenchResult is a struct that contains the benchmark results
type BenchResult struct {
	Throughput  float64
	Counter     int
	Errors      int
	Omitted     int
	Latency     *hdrhistogram.Histogram
	TotalTime   time.Duration
	Threads     int
	Connections int
}

// BenchmarkCmd is a main function helper that runs the provided target function using the commandline arguments
func BenchmarkCmd(target RequestFunc, params Benchmark) {
	fmt.Printf("Running %s test @ %s\n", params.Duration, params.URL)
	fmt.Printf("  %d threads and %d connections\n", params.Threads, params.Connections)
	result := params.Run()
	PrintBenchResult(params, result)
}

// arrayFlags is a type that implements the flag.Value interface
type arrayFlags []string

// String is a method that returns a string representation of the arrayFlags
func (i *arrayFlags) String() string {
	return "my string representation"
}

// Set is a method that sets the value of the arrayFlags
func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

// parseHeaders is a function that parses the headers
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

// localResult is a struct that contains the local results
type localResult struct {
	errors  int
	counter int
	latency *hdrhistogram.Histogram
}

// executioner is a struct that contains the executioner parameters
type executioner struct {
	eventsGenerator
	benchmark Benchmark
	results   chan localResult
	startTime time.Time
}

// eventsGenerator is a struct that contains the events generator parameters
type eventsGenerator struct {
	lock      sync.Mutex
	eventsBuf chan time.Time
	doneCtx   context.Context
	cancel    context.CancelFunc
	// omitted value is valid only after the execution is done
	omitted int
}

// Run is a method that runs the executioner
func (b Benchmark) Run() BenchResult {
	execution := b.newExecution()
	execution.generateEvents(b.Rate, 2*b.Connections*b.Threads) // coordinated omission effect
	for i := 0; i < b.Connections*b.Threads; i++ {              // coordinated omission effect
		go execution.sendRequests()
	}

	execution.awaitDone()

	return execution.summarizeResults()
}

// newExecution is a method that creates a new executioner
func (b Benchmark) newExecution() *executioner {
	return &executioner{
		eventsGenerator: newEventsGenerator(b.Duration, int(b.Rate*10) /*10 sec buffer*/),
		benchmark:       b,
		results:         make(chan localResult, b.Connections*b.Threads), // coordinated omission effect
		startTime:       time.Now(),
	}
}

// newEventsGenerator is a function that creates a new events generator
func newEventsGenerator(duration time.Duration, bufSize int) eventsGenerator {
	doneCtx, cancel := context.WithTimeout(context.Background(), duration)
	return eventsGenerator{
		lock:      sync.Mutex{},
		doneCtx:   doneCtx,
		cancel:    cancel,
		eventsBuf: make(chan time.Time, bufSize),
	}
}

// generateEvents is a method that generates events
func (e *eventsGenerator) generateEvents(throughput float64, burstSize int) {
	go func() {
		omitted := 0
		rateLimiter := rate.NewLimiter(rate.Limit(throughput), burstSize)
		for err := rateLimiter.Wait(e.doneCtx); err == nil; err = rateLimiter.Wait(e.doneCtx) {
			select {
			case e.eventsBuf <- time.Now():
			default:
				omitted++
			}
		}

		close(e.eventsBuf)
		e.lock.Lock()
		e.omitted = omitted
		e.lock.Unlock()
	}()
}

// omittedCount is a method that returns the number of omitted events
func (e *eventsGenerator) omittedCount() int {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.omitted
}

// awaitDone is a method that waits for the done context to be done
func (e *eventsGenerator) awaitDone() {
	<-e.doneCtx.Done()
	e.cancel()
}

// sendRequests is a method that sends requests
func (e *executioner) sendRequests() {
	res := localResult{
		errors:  0,
		counter: 0,
		latency: createHistogram(),
	}

	client := &http.Client{Timeout: e.benchmark.Timeout}                  // apply timeout
	req, err := http.NewRequest(e.benchmark.Method, e.benchmark.URL, nil) // generate request
	if err != nil {
		fmt.Println("Error creating request:", err) // error handling
		res.errors++                                // request creation error is also counted as an error
		e.results <- res
		return
	}

	if e.benchmark.Body != "" {
		req.Body = io.NopCloser(strings.NewReader(e.benchmark.Body)) // set request body
	}

	for key, value := range e.benchmark.Headers {
		req.Header.Set(key, value) // set headers
	}

	done := false
	for !done {
		select {
		case <-e.doneCtx.Done():
			done = true
		case _, ok := <-e.eventsBuf:
			if ok {
				res.counter++
				startTime := time.Now()          // record request start time
				resp, err := client.Do(req)      // send request
				elapsed := time.Since(startTime) // measure request time

				if err != nil {
					res.errors++
					log.Println("HTTP request error:", err) // output error log
				} else {
					if resp != nil {
						defer resp.Body.Close()
						if _, err := io.Copy(io.Discard, resp.Body); err != nil { // discard response body
							log.Println("Error reading response body:", err) // output error log
							res.errors++                                     // response body read error is also counted as an error
						}
						if resp.StatusCode >= 400 {
							res.errors++ // status code 400 or higher is also counted as an error
						}
					} else {
						res.errors++ // response is nil, so it is also counted as an error
					}
				}

				if err := res.latency.RecordValue(int64(elapsed)); err != nil { // record latency
					log.Println("failed to record latency", err) // output error log
				}
			} else {
				done = true
			}
		}
	}

	e.results <- res
}

// summarizeResults is a method that summarizes the results
func (e *executioner) summarizeResults() BenchResult {
	counter := 0
	errors := 0
	latency := createHistogram()

	for i := 0; i < e.benchmark.Connections*e.benchmark.Threads; i++ { // consider thread number
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
		Threads:     e.benchmark.Threads,     // include thread number in result
		Connections: e.benchmark.Connections, // include connection number in result
	}
}

// createHistogram creates a new histogram with a range of 0 to 1 minute and a precision of 3
func createHistogram() *hdrhistogram.Histogram {
	return hdrhistogram.New(0, int64(time.Minute), 3)
}

// PrintBenchResult is a function that prints the benchmark results
func PrintBenchResult(params Benchmark, result BenchResult) { // add params as an argument
	fmt.Printf("\n")
	fmt.Printf("Running %s test @ %s\n", params.Duration, params.URL)
	fmt.Printf("  %d threads and %d connections\n", params.Threads, params.Connections)

	if result.Errors > 0 || result.Omitted > 0 {
		fmt.Printf("  Socket errors: %d reads, %d writes, %d connects, %d timeouts\n", 0, 0, 0, result.Errors)
		if result.Omitted > 0 {
			fmt.Printf("  Non-2xx or 3xx responses: %d\n", result.Errors)
			fmt.Printf("  Omitted requests: %d\n", result.Omitted)
		}

	}

	fmt.Println()
	fmt.Println("Thread Stats   Avg      Stdev     Max   +/- Stdev")

	// calculate average latency, standard deviation, and maximum latency
	avgLatency := time.Duration(result.Latency.Mean())
	stdDevLatency := time.Duration(result.Latency.StdDev())
	maxLatency := time.Duration(result.Latency.Max())

	// calculate +/- Stdev
	plusMinusStdev := float64(stdDevLatency) / float64(avgLatency) * 100.0

	fmt.Printf("  Latency   %8s %8s %8s %6.2f%%\n",
		avgLatency,
		stdDevLatency,
		maxLatency,
		plusMinusStdev,
	)
	fmt.Printf("  Req/Sec    %8.2f %8.2f %8.2f %6.2f%%\n", // match wrk2 output format (Req/Sec's Stdev, Max are not implemented, so the average value is displayed)
		result.Throughput/float64(params.Threads), // average Req/Sec per thread (approximate value)
		0.0, // Stdev (not implemented)
		result.Throughput/float64(params.Threads), // Max (not implemented, so the average value is displayed)
		0.0, // +/- Stdev (not implemented)
	)
	fmt.Println()
	fmt.Println("Latency Distribution:")
	printHistogram(result.Latency)

	fmt.Printf("\n")
	fmt.Printf("  Requests/sec: %10.2f\n", result.Throughput)
	// Transfer/sec は未実装
	fmt.Printf("  Transfer/sec: %10.2fMB\n", 0.0) // match wrk2 output format (not implemented, so 0.0MB is displayed)
}

// printHistogram is a function that prints the histogram
func printHistogram(hist *hdrhistogram.Histogram) {

	fmt.Println("    50%    ", time.Duration(hist.ValueAtQuantile(50.0)))
	fmt.Println("    75%    ", time.Duration(hist.ValueAtQuantile(75.0)))
	fmt.Println("    90%    ", time.Duration(hist.ValueAtQuantile(90.0)))
	fmt.Println("    99%    ", time.Duration(hist.ValueAtQuantile(99.0)))
	fmt.Println("   99.9%    ", time.Duration(hist.ValueAtQuantile(99.9)))
	fmt.Println("  99.99%    ", time.Duration(hist.ValueAtQuantile(99.99)))
	fmt.Println("   100%    ", time.Duration(hist.ValueAtQuantile(100.0)))
}

// main is the main function
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
		fmt.Println("wrk3 modified by Okamyuji (Go version)") // display version information
		os.Exit(0)
	}

	if len(flag.Args()) < 1 {
		fmt.Println("Usage: wrk3 [options] <url>") // display usage information
		flag.PrintDefaults()
		os.Exit(1)
	}

	url := flag.Args()[0]
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		fmt.Println("Error: URL must start with http:// or https://") // fix error message
		os.Exit(1)
	}

	// create HTTP request function
	requestFunc := func() error {
		return nil // request sending process is moved to sendRequests
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
		URL:          url, // set URL to Benchmark struct
		SendRequest:  requestFunc,
		PrintLatency: printLatency, // set latency detail display flag
	}

	BenchmarkCmd(requestFunc, benchParams)
}
