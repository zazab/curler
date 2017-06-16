package main

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/kovetskiy/lorg"
)

const (
	usage = `curler

Usage:
    curler [options] <addr>

Options:
    -T <timeout>  connection timeout [default: 1s]
    -t <threads>  number of threads [default: 1]
    -c <cycles>   number of requests to perform [default: 100]
	-d <delay>    time to sleep before starting threads [default: 0s]
    <addr>        url to retrieve
`
	resultFilePath = "/app/results"
)

type requestStat struct {
	time time.Duration
	read int64
	err  error
}

var logger = lorg.NewLog()

func main() {
	args, err := docopt.Parse(usage, nil, true, "curler v1", false, true)
	if err != nil {
		logger.Fatalf("can't parse args: %s", err)
	}

	cycles, err := strconv.Atoi(args["-c"].(string))
	if err != nil {
		logger.Fatalf("can't parse cycles: %s", err)
	}

	threads, err := strconv.Atoi(args["-t"].(string))
	if err != nil {
		logger.Fatalf("can't parse threads '%s': %s", args["-t"].(string), err)
	}

	timeout, err := time.ParseDuration(args["-T"].(string))
	if err != nil {
		logger.Fatalf(
			"can't parse timeout '%s': %s", args["-T"].(string), err,
		)
	}

	url := args["<addr>"].(string)

	delay, err := time.ParseDuration(args["-d"].(string))
	if err != nil {
		logger.Fatalf(
			"can't parse delay '%s': %s", args["-d"].(string), err,
		)
	}

	if delay > 0 {
		logger.Infof("waiting %s before starting threads", delay)
		time.Sleep(delay)
	}

	wg := &sync.WaitGroup{}
	wg.Add(threads)

	stats := make(chan requestStat, threads)

	startCollector(stats, int64(threads*cycles), wg)
	for i := 0; i < threads; i++ {
		startThread(url, cycles, timeout, stats, wg)
	}
	logger.Infof("started %d curler threads", threads)

	wg.Wait()

	wg.Add(1)
	close(stats)
	wg.Wait()
}

type sortDurations []time.Duration

func (v sortDurations) Len() int      { return len(v) }
func (v sortDurations) Swap(i, j int) { v[i], v[j] = v[j], v[i] }
func (v sortDurations) Less(i, j int) bool {
	return v[i] < v[j]
}

func appendIfMissing(slice []reflect.Type, t reflect.Type) []reflect.Type {
	for _, storedType := range slice {
		if storedType == t {
			return slice
		}
	}

	return append(slice, t)
}

func toKilobyte(bytes int64) float64 {
	return float64(bytes) / 1024
}

func startCollector(
	stats <-chan requestStat,
	expected int64,
	wg *sync.WaitGroup,
) {
	name := os.Getenv("TEST_NAME")
	recordsFile, err := os.OpenFile(
		"/app/records.csv",
		os.O_CREATE|os.O_APPEND|os.O_WRONLY,
		0600,
	)
	if err != nil {
		logger.Fatalf("can't open file for records: %s", err)
	}

	logger.Info("starting collector thread")
	go func() {
		defer wg.Done()
		var (
			times     = []time.Duration{}
			errors    = map[string]int{}
			totalTime time.Duration
			totalRead int64
			timedOut  int64
		)

		ticker := time.NewTicker(10 * time.Second)

		go func() {
			for range ticker.C {
				writeResults(
					name,
					append(times),
					timedOut,
					expected,
					toKilobyte(totalRead),
					false,
				)
			}
		}()

		for stat := range stats {
			writeRecord(recordsFile, name, stat.time)
			if stat.err != nil {
				switch err := stat.err.(type) {
				case *url.Error:
					if err.Timeout() {
						timedOut++
					} else {
						errors = addError(errors, err)
					}
				default:
					errors = addError(errors, err)
				}
			} else {
				totalTime += stat.time
				times = append(times, stat.time)
				totalRead += stat.read
			}
		}

		ticker.Stop()

		writeResults(
			name,
			times,
			timedOut,
			expected,
			toKilobyte(totalRead),
			true,
		)

		if len(errors) > 0 {
			logger.Error("Encountered errors")
			for msg, cnt := range errors {
				logger.Errorf("%d - %s", cnt, msg)
			}
		}

		if int64(len(times)) < expected {
			logger.Fatalf(
				"%d succeed requests, expected %d",
				len(times), expected,
			)
		}
	}()
}

func startThread(
	url string,
	cycles int,
	timeout time.Duration,
	stats chan<- requestStat,
	wg *sync.WaitGroup,
) {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
		},
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		logger.Errorf("can't open '%s': %s", os.DevNull, err)
		return
	}

	go func() {
		defer wg.Done()

		for i := 0; i < cycles; i++ {
			stat := requestStat{}

			request, err := http.NewRequest("GET", url, nil)
			if err != nil {
				stat.err = err
				stats <- stat
				continue
			}

			request.Close = true

			start := time.Now()

			response, err := client.Do(request)
			if err != nil {
				stat.err = err
				stats <- stat
				continue
			}

			bytes, err := io.Copy(devNull, response.Body)
			if err != nil {
				logger.Errorf("can't write to /dev/null: %s", err)
				stat.err = err
			}
			stat.time = time.Now().Sub(start)
			stat.read = bytes

			stats <- stat
		}
	}()
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration.Nanoseconds()) / 1000000
}

func writeRecord(
	file io.Writer,
	name string,
	latency time.Duration,
) {
	// nolint: gas
	fmt.Fprintf(
		file,
		"%s,%0.2f\n",
		name, milliseconds(latency),
	)
}

func writeResults(
	name string,
	times []time.Duration,
	timedOut, expected int64,
	totalRead float64,
	final bool,
) {
	file, err := os.OpenFile(
		resultFilePath,
		os.O_WRONLY|os.O_CREATE|os.O_APPEND,
		0600,
	)
	if err != nil {
		logger.Fatalf("can't open result file '%s': %s", resultFilePath, err)
	}

	defer file.Close()

	var (
		totalTime       time.Duration
		successRequests = int64(len(times))
		//errorRequests   = expected - successRequests - timedOut
	)

	if successRequests == 0 {
		logger.Error("no success records")
		return
	}

	for _, time := range times {
		totalTime += time
	}

	average := milliseconds(
		totalTime / time.Duration(successRequests),
	)
	logger.Infof("requests: %d; average: %.2f", successRequests, average)

	var dev float64

	for _, time := range times {
		diff := milliseconds(time) - average
		dev += diff * diff
	}

	dev = dev / float64(successRequests-1)
	dev = math.Sqrt(dev)

	sort.Sort(sortDurations(times))

	var (
		bandwidth = totalRead / totalTime.Seconds()
		p95       = percentile(times, 0.95)
		p99       = percentile(times, 0.99)
		p100      = percentile(times, 1)
		rps       = float64(successRequests) / totalTime.Seconds()
	)

	// nolint: gas
	fmt.Fprintf(
		file,
		"%s,%d,%d,"+
			"%.3f,%.3f,%.3f,%.3f,%.3f,"+
			"%.3f,%.3f,%.3f,%.3f,%t\n",
		name, successRequests, timedOut,
		average, milliseconds(p95), milliseconds(p99), milliseconds(p100), dev,
		totalTime.Seconds(), totalRead, bandwidth, rps, final,
	)
}

func percentile(times []time.Duration, p float64) time.Duration {
	position := int(float64(len(times)) * p)
	if position == len(times) {
		position--
	}

	return times[position]
}

func addError(errors map[string]int, err error) map[string]int {
	msg := err.Error()
	if _, ok := errors[msg]; !ok {
		errors[msg] = 0
	}
	errors[msg]++
	return errors
}
