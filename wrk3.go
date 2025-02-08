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

type RequestFunc func() error

type Benchmark struct {
	Concurrency int
	Throughput  float64
	Duration    time.Duration
	Method      string
	Headers     map[string]string
	Body        string
	SendRequest RequestFunc
}

type BenchResult struct {
	Throughput float64
	Counter    int
	Errors     int
	Omitted    int
	Latency    *hdrhistogram.Histogram
	TotalTime  time.Duration
}

// BenchmarkCmd is a main function helper that runs the provided target function using the commandline arguments
func BenchmarkCmd(target RequestFunc, params Benchmark) {
	fmt.Printf("running benchmark for %v...\n", params.Duration)
	result := params.Run()
	PrintBenchResult(params.Throughput, params.Duration, result)
}

type arrayFlags []string

func (i *arrayFlags) String() string {
	return "my string representation"
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

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

type localResult struct {
	errors  int
	counter int
	latency *hdrhistogram.Histogram
}

type executioner struct {
	eventsGenerator
	benchmark Benchmark
	results   chan localResult
	startTime time.Time
}

type eventsGenerator struct {
	lock      sync.Mutex
	eventsBuf chan time.Time
	doneCtx   context.Context
	cancel    context.CancelFunc
	// omitted value is valid only after the execution is done
	omitted int
}

func (b Benchmark) Run() BenchResult {
	execution := b.newExecution()
	execution.generateEvents(b.Throughput, 2*b.Concurrency)
	for i := 0; i < b.Concurrency; i++ {
		go execution.sendRequests()
	}

	execution.awaitDone()

	return execution.summarizeResults()
}

func (b Benchmark) newExecution() *executioner {
	return &executioner{
		eventsGenerator: newEventsGenerator(b.Duration, int(b.Throughput*10) /*10 sec buffer*/),
		benchmark:       b,
		results:         make(chan localResult, b.Concurrency),
		startTime:       time.Now(),
	}
}

func newEventsGenerator(duration time.Duration, bufSize int) eventsGenerator {
	doneCtx, cancel := context.WithTimeout(context.Background(), duration)
	return eventsGenerator{
		lock:      sync.Mutex{},
		doneCtx:   doneCtx,
		cancel:    cancel,
		eventsBuf: make(chan time.Time, bufSize),
	}
}

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

func (e *eventsGenerator) omittedCount() int {
	e.lock.Lock()
	defer e.lock.Unlock()
	return e.omitted
}

func (e *eventsGenerator) awaitDone() {
	<-e.doneCtx.Done()
	e.cancel()
}

func (e *executioner) sendRequests() {
	res := localResult{
		errors:  0,
		counter: 0,
		latency: createHistogram(),
	}

	done := false
	for !done {
		select {
		case <-e.doneCtx.Done():
			done = true
		case t, ok := <-e.eventsBuf:
			if ok {
				res.counter++
				err := e.benchmark.SendRequest()
				if err != nil {
					res.errors++
				}

				if err = res.latency.RecordValue(int64(time.Since(t))); err != nil {
					log.Println("failed to record latency", err)
				}
			} else {
				done = true
			}
		}
	}

	e.results <- res
}

func (e *executioner) summarizeResults() BenchResult {
	counter := 0
	errors := 0
	latency := createHistogram()

	for i := 0; i < e.benchmark.Concurrency; i++ {
		localRes := <-e.results
		counter += localRes.counter
		errors += localRes.errors
		latency.Merge(localRes.latency)
	}

	totalTime := time.Since(e.startTime)

	return BenchResult{
		Throughput: float64(counter) / totalTime.Seconds(),
		Counter:    counter,
		Errors:     errors,
		Omitted:    e.omittedCount(),
		Latency:    latency,
		TotalTime:  totalTime,
	}
}

func createHistogram() *hdrhistogram.Histogram {
	return hdrhistogram.New(0, int64(time.Minute), 3)
}

func PrintBenchResult(throughput float64, duration time.Duration, result BenchResult) {
	fmt.Println("benchmark results:")
	fmt.Println("total duration: ", result.TotalTime, "(target duration:", duration, ")")
	fmt.Println("total requests: ", result.Counter)
	fmt.Println("errors: ", result.Errors)
	fmt.Println("omitted requests: ", result.Omitted)
	fmt.Println("throughput: ", result.Throughput, "(target throughput:", throughput, ")")
	fmt.Println("latency distribution:")
	printHistogram(result.Latency)
}

func printHistogram(hist *hdrhistogram.Histogram) {
	brackets := hist.CumulativeDistribution()

	fmt.Println("Quantile    | Count     | Value ")
	fmt.Println("------------+-----------+-------------")

	for _, q := range brackets {
		fmt.Printf("%-08.3f    | %-09d | %v\n", q.Quantile, q.Count, time.Duration(q.ValueAt))
	}
}

func main() {

	if len(os.Args) < 4 {
		fmt.Println("Usage: go run ./wrk3.go -c <concurrency> -t <throughput> -d <duration> [options] <url>")
		fmt.Println("Example: go run ./wrk3.go -c 10 -t 1000 https://google.com")
		fmt.Println("\nOptions:")
		fmt.Println("  -c int: Level of benchmark concurrency (default: 10)")
		fmt.Println("                        Level of benchmark concurrency (default: 10)")
		fmt.Println("  -t float64: Target benchmark throughput (default: 10000)")
		fmt.Println("  -d duration: Benchmark time period (default: 20s)")
		fmt.Println("  -m string: HTTP method (GET, POST, PUT, DELETE, etc.) (default: GET)")
		fmt.Println("  -h string: HTTP header to send, format is 'key:value'")
		fmt.Println("  -b string: Request body to send")
		os.Exit(1)
	}

	var concurrency int
	var throughput float64
	var duration time.Duration
	var method string
	var headers arrayFlags
	var body string

	flag.IntVar(&concurrency, "c", 10, "level of benchmark concurrency")
	flag.Float64Var(&throughput, "t", 10000, "target benchmark throughput")
	flag.DurationVar(&duration, "d", 20*time.Second, "benchmark time period")
	flag.StringVar(&method, "m", "GET", "HTTP method (GET, POST, PUT, DELETE, etc.)")
	flag.Var(&headers, "h", "HTTP header to send, format is 'key:value'")
	flag.StringVar(&body, "b", "", "Request body to send")
	flag.Parse()

	url := os.Args[len(os.Args)-1]
	if !strings.Contains(url, "http") {
		fmt.Println("Error: URL is required")
		os.Exit(1)
	}

	// create HTTP request function
	requestFunc := func() error {
		client := &http.Client{}
		req, err := http.NewRequest(method, url, nil) // Body is nil for now
		if err != nil {
			fmt.Println("Error creating request:", err)
			return err
		}

		if body != "" {
			req.Body = io.NopCloser(strings.NewReader(body))
		}

		headerMap := parseHeaders(headers)
		for key, value := range headerMap {
			req.Header.Set(key, value)
		}

		resp, err := client.Do(req)

		if err != nil {
			fmt.Println("HTTP request error:", err)
			return err
		}

		if resp != nil {
			defer resp.Body.Close()
		}

		// discard response body (add processing if needed)
		_, err = io.Copy(io.Discard, resp.Body)
		if err != nil {
			fmt.Println("Error reading response body:", err)
			return err
		}

		fmt.Println("Status code:", resp.StatusCode)
		if resp.StatusCode >= 400 {
			return fmt.Errorf("HTTP error: %s", resp.Status)
		}

		return nil
	}

	benchParams := Benchmark{
		Concurrency: concurrency,
		Throughput:  throughput,
		Duration:    duration,
		Method:      method,
		Headers:     parseHeaders(headers),
		Body:        body,
		SendRequest: requestFunc,
	}

	BenchmarkCmd(requestFunc, benchParams)
}
