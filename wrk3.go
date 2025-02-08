package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
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
func BenchmarkCmd(target RequestFunc) {
	var concurrency int
	var throughput float64
	var duration time.Duration

	flag.IntVar(&concurrency, "concurrency", 10, "level of benchmark concurrency")
	flag.Float64Var(&throughput, "throughput", 10000, "target benchmark throughput")
	flag.DurationVar(&duration, "duration", 20*time.Second, "benchmark time period")
	flag.Parse()

	fmt.Printf("running benchmark for %v...\n", duration)
	b := Benchmark{
		Concurrency: concurrency,
		Throughput:  throughput,
		Duration:    duration,
		SendRequest: target,
	}
	result := b.Run()
	PrintBenchResult(throughput, duration, result)
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
	if len(os.Args) < 5 {
		fmt.Println("Usage: go run ./wrk3.go -concurrency <concurrency> -throughput <throughput> -duration <duration> <url>")
		fmt.Println("Example: go run ./wrk3.go -concurrency 10 -throughput 1000 https://google.com")
		os.Exit(1)
	}

	url := os.Args[len(os.Args)-1] // URL をコマンドライン引数から取得

	// HTTP リクエストを実行する RequestFunc を作成
	requestFunc := func() error {
		// gosec G107 警告を無視 (URL 検証済みのため)
		resp, err := http.Get(url) // #nosec G107
		if err != nil {
			fmt.Println("HTTP Get error:", err)
			return err
		}
		defer resp.Body.Close()

		// レスポンス Body を読み捨てる (必要に応じて処理を追加)
		_, err = io.Copy(ioutil.Discard, resp.Body)
		if err != nil {
			fmt.Println("Error reading response body:", err)
			return err
		}

		if resp.StatusCode >= 400 {
			return fmt.Errorf("HTTP error: %s", resp.Status)
		}

		return nil
	}

	BenchmarkCmd(requestFunc) // BenchmarkCmd を呼び出す
}
