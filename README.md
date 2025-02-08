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

### Usage Example

```sh
go run ./wrk3.go -c 10 -t 1000 -d 30s -m POST -h "Content-Type: application/json" -b '{"key":"value"}' http://localhost:8080/endpoint
```

The tool maintains the original wrk2's coordinated omission prevention while adding these HTTP-specific features.
