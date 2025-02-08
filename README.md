# wrk3

[![Build Status](https://travis-ci.org/AppsFlyer/wrk3.svg?branch=master)](https://travis-ci.org/AppsFlyer/wrk3)
[![Coverage Status](https://coveralls.io/repos/github/AppsFlyer/wrk3/badge.svg?branch=master)](https://coveralls.io/github/AppsFlyer/wrk3?branch=master)
[![Go Report Card](https://goreportcard.com/badge/github.com/AppsFlyer/wrk3)](https://goreportcard.com/report/github.com/AppsFlyer/wrk3)
[![Godocs](https://img.shields.io/badge/golang-documentation-blue.svg)](https://godoc.org/github.com/AppsFlyer/wrk3)

A golang generic benchmarking tool based mostly on Gil Tene's wrk2, only rewritten in go, and extended to allow running arbitrary protocols.

## Recent Updates

This fork includes significant improvements to the HTTP benchmarking capabilities:

- Enhanced command-line interface with support for HTTP method specification (`-m`)
- Custom header support (`-h "Key: Value"`)
- Request body support (`-b`) for POST/PUT operations
- Improved error handling and status code validation
- Cleaner dependency injection pattern for benchmark parameters
- Detailed latency distribution reporting

## Features

- Rate limiting with coordinated omission handling
- Detailed latency statistics with HDR Histogram
- Multiple connection and thread support
- Configurable HTTP methods, headers, and body
- Timeout handling for requests
- Comprehensive error reporting

## Installation

```sh
go install github.com/okamyuji/wrk3@latest
```

## Usage

```sh
wrk3 [options] <url>
```

### Command Line Options

```sh
-c, --connections int
```

Connections to keep open (default: 10)

```sh
-t, --threads int
```

Number of threads to use (default: 2)

```sh
-r, --rate float
```

Target request rate in requests/sec (default: 10000)

```sh
-d, --duration duration
```

Duration of test (default: 10s)

```sh
-T, --timeout duration
```

Socket/request timeout (default: 0s)

```sh
-m, --method string
```

HTTP method (default: "GET")

```sh
-H, --header string
```

Add header to request (can be specified multiple times)

```sh
-b, --body string
```

Request body

```sh
--latency
```

Print latency statistics

```sh
-v, --version
```

Print version information

## Example

Basic benchmark with rate limiting

```sh
wrk3 -c 10 -t 2 -r 100 -d 30s "http://localhost:8080/post"
```

Benchmark with custom headers and method

```sh
wrk3 -c 50 -t 4 -H "Authorization: Bearer token" -m POST "http://localhost:8080/post"
```

## Output

The tool provides detailed statistics including

- Request throughput
- Total requests processed
- Error count
- Latency distribution (when --latency is enabled)
- Connection and thread counts
- Test duration

## Testing

The test suite includes several key test cases

- Basic HTTP benchmarking functionality
- Rate limiting verification
- High latency server handling
- Request error handling
- Result summarization

To run tests

```sh
make test
```

## Implementation Details

- Uses Go's `rate.Limiter` for precise request rate control
- Implements HDR Histogram for accurate latency measurements
- Handles coordinated omission through event generation
- Supports concurrent connections through goroutines
- Provides comprehensive error handling and reporting

## License

MIT License

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.
