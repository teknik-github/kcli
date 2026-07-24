// Package bench runs a small HTTP load test and reports latency statistics.
// It knows nothing about Kubernetes: the caller supplies a plain URL, which for
// an in-cluster target is the local end of a port-forward.
package bench

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options describes one load test. Requests and Duration are alternatives: with
// Requests > 0 the run stops after that many requests, otherwise it runs for
// Duration.
type Options struct {
	URL         string
	Method      string
	Requests    int
	Concurrency int
	Duration    time.Duration
	Timeout     time.Duration     // per-request timeout
	Host        string            // Host header override (Ingress targets)
	Headers     map[string]string // extra request headers
	Body        string
	Insecure    bool // skip TLS verification (self-signed ingress certs)
}

func (o *Options) applyDefaults() {
	if o.Method == "" {
		o.Method = http.MethodGet
	}
	if o.Concurrency < 1 {
		o.Concurrency = 1
	}
	if o.Requests <= 0 && o.Duration <= 0 {
		o.Duration = 10 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Requests > 0 && o.Concurrency > o.Requests {
		o.Concurrency = o.Requests // more workers than requests just idles them
	}
}

// Result is the outcome of a run. Latencies cover every completed request,
// including ones that failed with a status code (a 500 still has a latency);
// transport errors contribute to Failed and Errors but carry no useful timing.
type Result struct {
	Elapsed time.Duration
	Total   int
	Success int // completed with a status below 400
	Failed  int // transport error, or a status of 400 and up
	Bytes   int64
	RPS     float64

	Min, Mean, P50, P90, P95, P99, Max time.Duration

	Codes  map[int]int    // status code -> count
	Errors map[string]int // transport error -> count

	lat []time.Duration // every measured latency, ascending
}

// Run executes the load test, calling progress (if non-nil) every 250ms with
// the running completed/failed counts. It returns once every worker has
// stopped; cancelling ctx ends the run early and still returns what was
// measured.
func Run(ctx context.Context, o Options, progress func(done, failed int)) (*Result, error) {
	o.applyDefaults()
	u, err := url.ParseRequestURI(o.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("not an http(s) url: %q", o.URL)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if o.Requests <= 0 {
		var stop context.CancelFunc
		runCtx, stop = context.WithTimeout(runCtx, o.Duration)
		defer stop()
	}

	// One connection pool sized to the worker count, so keep-alive is measured
	// rather than a fresh TCP (and TLS) handshake per request.
	tr := &http.Transport{
		MaxIdleConns:        o.Concurrency * 2,
		MaxIdleConnsPerHost: o.Concurrency * 2,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: o.Insecure}, // #nosec G402 — opt-in, for self-signed cluster certs
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport: tr,
		Timeout:   o.Timeout,
		// Don't follow redirects: the point is to measure the endpoint asked for,
		// not wherever it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// shard is one worker's tally, merged at the end — so the hot path takes no
	// locks and the workers never contend.
	type shard struct {
		lat             []time.Duration
		codes           map[int]int
		errs            map[string]int
		bytes           int64
		success, failed int
	}
	shards := make([]shard, o.Concurrency)

	var completed, failedN atomic.Int64
	var left atomic.Int64
	left.Store(int64(o.Requests))

	if progress != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			t := time.NewTicker(250 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					progress(int(completed.Load()), int(failedN.Load()))
				}
			}
		}()
	}

	start := time.Now()
	var wg sync.WaitGroup
	for i := range shards {
		wg.Add(1)
		go func(s *shard) {
			defer wg.Done()
			s.codes = map[int]int{}
			s.errs = map[string]int{}
			for {
				if runCtx.Err() != nil {
					return
				}
				if o.Requests > 0 && left.Add(-1) < 0 {
					return
				}
				t0 := time.Now()
				code, n, err := do(runCtx, client, &o)
				elapsed := time.Since(t0)
				if err != nil {
					if runCtx.Err() != nil {
						return // torn down mid-flight: not a measurement
					}
					s.failed++
					s.errs[errKey(err)]++
					failedN.Add(1)
					completed.Add(1)
					continue
				}
				s.lat = append(s.lat, elapsed)
				s.bytes += n
				s.codes[code]++
				if code < 400 {
					s.success++
				} else {
					s.failed++
					failedN.Add(1)
				}
				completed.Add(1)
			}
		}(&shards[i])
	}
	wg.Wait()

	res := &Result{
		Elapsed: time.Since(start),
		Codes:   map[int]int{},
		Errors:  map[string]int{},
	}
	for i := range shards {
		s := &shards[i]
		res.lat = append(res.lat, s.lat...)
		res.Bytes += s.bytes
		for c, n := range s.codes {
			res.Codes[c] += n
		}
		for e, n := range s.errs {
			res.Errors[e] += n
		}
		res.Success += s.success
		res.Failed += s.failed
	}
	res.Total = res.Success + res.Failed
	res.summarise()
	return res, nil
}

// do issues one request and drains the body, so the connection can be reused
// and the transferred bytes are counted.
func do(ctx context.Context, c *http.Client, o *Options) (int, int64, error) {
	var body io.Reader
	if o.Body != "" {
		body = strings.NewReader(o.Body)
	}
	req, err := http.NewRequestWithContext(ctx, o.Method, o.URL, body)
	if err != nil {
		return 0, 0, err
	}
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}
	if o.Host != "" {
		req.Host = o.Host // the Host header, not the address dialled
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, n, nil
}

// summarise derives the reported statistics from the collected latencies.
func (r *Result) summarise() {
	if r.Elapsed > 0 {
		r.RPS = float64(r.Total) / r.Elapsed.Seconds()
	}
	if len(r.lat) == 0 {
		return
	}
	sort.Slice(r.lat, func(i, j int) bool { return r.lat[i] < r.lat[j] })
	var sum time.Duration
	for _, d := range r.lat {
		sum += d
	}
	r.Mean = sum / time.Duration(len(r.lat))
	r.Min, r.Max = r.lat[0], r.lat[len(r.lat)-1]
	r.P50 = percentile(r.lat, 50)
	r.P90 = percentile(r.lat, 90)
	r.P95 = percentile(r.lat, 95)
	r.P99 = percentile(r.lat, 99)
}

// Bucket is one bar of the latency distribution.
type Bucket struct {
	Lo, Hi time.Duration
	Count  int
}

// Histogram splits the measured latencies into n equal-width buckets between
// the fastest and slowest request. It returns nil when there is nothing (or
// nothing varied enough) to plot.
func (r *Result) Histogram(n int) []Bucket {
	if len(r.lat) == 0 || n < 1 || r.Max <= r.Min {
		return nil
	}
	width := float64(r.Max-r.Min) / float64(n)
	out := make([]Bucket, n)
	for i := range out {
		out[i].Lo = r.Min + time.Duration(width*float64(i))
		out[i].Hi = r.Min + time.Duration(width*float64(i+1))
	}
	for _, d := range r.lat {
		i := int(float64(d-r.Min) / width)
		if i >= n { // the slowest request lands exactly on the top edge
			i = n - 1
		}
		out[i].Count++
	}
	return out
}

// percentile returns the p-th percentile of an ascending slice (nearest-rank).
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// errKey normalises a transport error into a groupable label: *url.Error wraps
// every failure with the method and URL, which would make each one unique.
func errKey(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	s := err.Error()
	if len(s) > 60 {
		s = s[:59] + "…"
	}
	return s
}
