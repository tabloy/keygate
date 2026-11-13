package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type updateTestTransport func(*http.Request) (*http.Response, error)

func (f updateTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func updateTestResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestSystemUpdateFailureCanRetry(t *testing.T) {
	for _, failure := range []string{"network", "status", "json", "version"} {
		t.Run(failure, func(t *testing.T) {
			var calls atomic.Int32
			h := NewSystemHandler(nil)
			h.httpClient = &http.Client{Transport: updateTestTransport(func(*http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					switch failure {
					case "network":
						return nil, errors.New("controlled offline")
					case "status":
						return updateTestResponse(503, "unavailable"), nil
					case "json":
						return updateTestResponse(200, "{"), nil
					default:
						return updateTestResponse(200, `{"tag_name":"invalid-version"}`), nil
					}
				}
				return updateTestResponse(200, `{"tag_name":"v99.0.0"}`), nil
			})}
			first := h.checkLatestRelease(context.Background())
			if first.Error == "" {
				t.Fatal("failed read was presented as a successful comparison")
			}
			second := h.checkLatestRelease(context.Background())
			if second.Error != "" || second.Latest != "v99.0.0" || calls.Load() != 2 {
				t.Fatalf("retry did not recover: %+v, calls=%d", second, calls.Load())
			}
			third := h.checkLatestRelease(context.Background())
			if third != second || calls.Load() != 2 {
				t.Fatal("successful check was not cached")
			}
		})
	}
}

type updateJoinedContext struct {
	context.Context
	joined chan<- struct{}
}

func (c updateJoinedContext) Done() <-chan struct{} { c.joined <- struct{}{}; return c.Context.Done() }

func TestSystemUpdateConcurrentChecksShareFailure(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	h := NewSystemHandler(nil)
	h.httpClient = &http.Client{Transport: updateTestTransport(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil, errors.New("controlled offline")
	})}
	var workers sync.WaitGroup
	results := make(chan *updateInfo, 10)
	workers.Add(1)
	go func() { defer workers.Done(); results <- h.checkLatestRelease(context.Background()) }()
	<-entered
	joined := make(chan struct{}, 9)
	for i := 0; i < 9; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			results <- h.checkLatestRelease(updateJoinedContext{Context: context.Background(), joined: joined})
		}()
	}
	for i := 0; i < 9; i++ {
		<-joined
	}
	close(release)
	workers.Wait()
	close(results)
	for result := range results {
		if result.Error == "" {
			t.Fatal("missing shared failure")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent checks made %d upstream requests", calls.Load())
	}
	if h.checkLatestRelease(context.Background()).Error == "" || calls.Load() != 2 {
		t.Fatal("later intentional retry did not make a new request")
	}
}

func TestSystemUpdateNoPublishedReleasesIsCached(t *testing.T) {
	var calls atomic.Int32
	h := NewSystemHandler(nil)
	h.httpClient = &http.Client{Transport: updateTestTransport(func(*http.Request) (*http.Response, error) { calls.Add(1); return updateTestResponse(404, ""), nil })}
	first := h.checkLatestRelease(context.Background())
	if first.Error != "" || first.Available || first.Latest != first.Current {
		t.Fatalf("unexpected no-release result: %+v", first)
	}
	h.checkLatestRelease(context.Background())
	if calls.Load() != 1 {
		t.Fatal("no-release result was not cached")
	}
}
