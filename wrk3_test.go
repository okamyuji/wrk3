package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func createHTTPLoadFunction(url string, timeout time.Duration) func() error {
	client := &http.Client{
		Transport: &http.Transport{
			MaxConnsPerHost: 200,
		},
		Timeout: timeout,
	}

	return func() error {
		resp, err := client.Get(url)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}

		if err != nil {
			fmt.Println("error sending get request:", err)
		}
		return err
	}
}

func createHTTPServer(addr string) *http.Server {
	server := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			n, err := rand.Int(rand.Reader, big.NewInt(10))
			if err != nil {
				panic(err)
			}
			sleepDuration := time.Millisecond * time.Duration(n.Int64())
			time.Sleep(sleepDuration)
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		err := server.ListenAndServe()
		if err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	return server
}

func createSlowHTTPServer(addr string) *http.Server {
	server := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			time.Sleep(500 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		err := server.ListenAndServe()
		if err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	time.Sleep(500 * time.Millisecond) // wait for server to start
	return server
}

func TestBasicHttpBench(t *testing.T) {
	server := createHTTPServer(":8080")
	expectedThroughput := float64(1000)
	expectedDuration := 10 * time.Second
	benchResult := Benchmark{
		Connections: 10,
		Threads:     1,
		Rate:        expectedThroughput,
		Duration:    expectedDuration,
		SendRequest: createHTTPLoadFunction("http://localhost:8080/", 100*time.Millisecond),
		URL:         "http://localhost:8080/",
	}.Run()

	assert.Equal(t, expectedThroughput/100, math.Round(benchResult.Throughput/100), "Throughput")
	assert.Equal(t, expectedDuration, benchResult.TotalTime.Truncate(time.Second), "bench time")

	distribution := benchResult.Latency.CumulativeDistribution()
	assert.True(t, time.Duration(distribution[len(distribution)-1].ValueAt) < 100*time.Millisecond, "large percentiles are too large")

	_ = server.Shutdown(context.Background())
}

func TestHighLatencyServer(t *testing.T) {
	server := createSlowHTTPServer(":8082")
	defer func() {
		err := server.Shutdown(context.Background())
		if err != nil {
			t.Errorf("failed to shutdown server: %v", err)
		}
	}()

	time.Sleep(1 * time.Second)

	benchResult := Benchmark{
		Connections: 2,
		Threads:     1,
		Duration:    5 * time.Second,
		SendRequest: createHTTPLoadFunction("http://localhost:8082/", 2*time.Second),
		URL:         "http://localhost:8082/",
	}.Run()

	// verify server processing capability
	assert.LessOrEqual(t, benchResult.Throughput, 2.1,
		"throughput should be limited by server latency (500ms)")
}

func TestEventsGenerator(t *testing.T) {
	throughput := 1000.0
	expectedDuration := 50 * time.Millisecond
	generator := newEventsGenerator(expectedDuration, 10)
	generator.generateEvents(throughput, 5)
	generator.awaitDone()

	assert.Greater(t, generator.omittedCount(), 0,
		"should have omitted events due to buffer limitation")
}

func TestSendRequestsWithErrors(t *testing.T) {
	expectedErrors := 5
	events := make(chan time.Time, expectedErrors)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	e := executioner{
		results: make(chan localResult, 1),
		eventsGenerator: eventsGenerator{
			doneCtx:   ctx,
			cancel:    cancel,
			eventsBuf: events,
		},
		benchmark: Benchmark{
			SendRequest: func() error {
				return fmt.Errorf("test error")
			},
		},
		startTime: time.Now(),
	}

	for i := 0; i < expectedErrors; i++ {
		events <- time.Now()
	}
	close(events)

	go e.sendRequests()

	result := <-e.results
	assert.Equal(t, expectedErrors, result.errors, "errors")
	assert.Equal(t, expectedErrors, result.counter, "count")
}

func TestSendRequests(t *testing.T) {
	expectedResults := 5
	events := make(chan time.Time, expectedResults)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// テストサーバーを起動
	server := createHTTPServer(":8082")
	defer func() {
		err := server.Shutdown(context.Background())
		if err != nil {
			t.Errorf("failed to shutdown server: %v", err)
		}
	}()
	time.Sleep(10 * time.Millisecond)

	// executionerを作成
	e := executioner{
		results: make(chan localResult, 1),
		eventsGenerator: eventsGenerator{
			doneCtx:   ctx,
			cancel:    cancel,
			eventsBuf: events,
		},
		benchmark: Benchmark{
			SendRequest: createHTTPLoadFunction("http://localhost:8082/", 50*time.Millisecond),
			URL:         "http://localhost:8082/",
			Rate:        1, // レート制限を追加
		},
		startTime: time.Now(),
	}

	// prepare test data
	for i := 0; i < expectedResults; i++ {
		events <- time.Now()
	}
	close(events)

	go e.sendRequests()
	result := <-e.results
	assert.Equal(t, 0, result.errors, "errors")
	assert.Equal(t, expectedResults, result.counter, "count")
}

func TestSummarizeResults(t *testing.T) {
	expectedResults := 5
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// create histogram to record latency
	hist := createHistogram()
	for i := 0; i < expectedResults; i++ {
		err := hist.RecordValue(int64(time.Millisecond))
		assert.NoError(t, err)
	}

	// create executioner with minimal settings
	e := executioner{
		results: make(chan localResult, 1),
		eventsGenerator: eventsGenerator{
			doneCtx: ctx,
			cancel:  cancel,
		},
		benchmark: Benchmark{
			Duration:    50 * time.Millisecond,
			Threads:     1,
			Connections: 1,
		},
		startTime: time.Now().Add(-50 * time.Millisecond),
	}

	// prepare data to send to result channel
	totalHist := createHistogram()
	e.results <- localResult{
		counter: expectedResults,
		errors:  0,
		latency: totalHist,
	}

	// merge results to verification histogram
	totalHist.Merge(hist)
	close(e.results)

	// verify results
	result := e.summarizeResults()
	assert.Equal(t, expectedResults, result.Counter, "count")
	assert.Equal(t, expectedResults, int(result.Latency.TotalCount()), "histogram counter")
	assert.Greater(t, result.Throughput, 0.0, "throughput should be positive")
}
