// Copyright 2021 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collector

import (
	"bufio"
	"fmt"
	"io"
	"math"
	_ "net/http/pprof"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/statsd_exporter/pkg/mapper"
)

var invalidMetricChars = regexp.MustCompile("[^a-zA-Z0-9_:]")

type graphiteCollector struct {
	mapper             metricMapper
	sampleCh           chan *graphiteSample
	lineCh             chan string
	strictMatch        bool
	logger             log.Logger
	tagParseFailures   prometheus.Counter
	lastProcessed      prometheus.Gauge
	sampleExpiryMetric prometheus.Gauge
	sampleExpiry       time.Duration
	exposeTimestamps   bool
}

func NewGraphiteCollector(logger log.Logger, strictMatch bool, sampleExpiry time.Duration) *graphiteCollector {
	c := &graphiteCollector{
		sampleCh:    make(chan *graphiteSample, 50000000),
		lineCh:      make(chan string),
		strictMatch: strictMatch,
		logger:      logger,
		tagParseFailures: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "graphite_tag_parse_failures",
				Help: "Total count of samples with invalid tags",
			}),
		lastProcessed: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "graphite_last_processed_timestamp_seconds",
				Help: "Unix timestamp of the last processed graphite metric.",
			},
		),
		sampleExpiry: sampleExpiry,
		sampleExpiryMetric: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "graphite_sample_expiry_seconds",
				Help: "How long in seconds a metric sample is valid for.",
			},
		),
	}
	c.sampleExpiryMetric.Set(sampleExpiry.Seconds())
	go c.processLines()
	return c
}

func (c *graphiteCollector) ExposeTimestamps(exposeTimestamps bool) {
	c.exposeTimestamps = exposeTimestamps
}

func (c *graphiteCollector) ProcessReader(reader io.Reader) {
	lineScanner := bufio.NewScanner(reader)
	for {
		if ok := lineScanner.Scan(); !ok {
			break
		}
		c.lineCh <- lineScanner.Text()
	}
}

func (c *graphiteCollector) SetMapper(m metricMapper) {
	c.mapper = m
}

func (c *graphiteCollector) processLines() {
	for line := range c.lineCh {
		c.processLine(line)
	}
}

func (c *graphiteCollector) parseMetricNameAndTags(name string) (string, prometheus.Labels, error) {
	var err error

	labels := make(prometheus.Labels)

	parts := strings.Split(name, ";")
	parsedName := parts[0]

	tags := parts[1:]
	for _, tag := range tags {
		kv := strings.SplitN(tag, "=", 2)
		if len(kv) != 2 {
			// don't add this tag, continue processing tags but return an error
			c.tagParseFailures.Inc()
			err = fmt.Errorf("error parsing tag %s", tag)
			continue
		}

		k := kv[0]
		v := kv[1]
		labels[k] = v
	}

	return parsedName, labels, err
}

func gsplit(name string) map[string]string {
	split := strings.Split(name, ".")
	labels := make(map[string]string)
	for i, part := range split {
		labelKey := fmt.Sprintf("gsplit_%d", i)
		labels[labelKey] = part
	}
	if len(labels) > 0 {
		labels[fmt.Sprintf("gsplit_%d",len(labels) - 1)] += "."
	}

	return labels
}

func mergeLabels(s1 map[string]string, s2 map[string]string) map[string]string {
	ret := make(map[string]string)
	for k, v := range s1 {
		ret[k] = v
	}
	for k, v := range s2 {
		ret[k] = v
	}
	return ret
}

func (c *graphiteCollector) processLine(line string) {
	line = strings.TrimSpace(line)
	level.Debug(c.logger).Log("msg", "Incoming line", "line", line)

	parts := strings.Split(line, " ")
	if len(parts) != 3 {
		level.Info(c.logger).Log("msg", "Invalid part count", "parts", len(parts), "line", line)
		return
	}

	originalName := parts[0]

	parsedName, labels, err := c.parseMetricNameAndTags(originalName)
	if err != nil {
		level.Debug(c.logger).Log("msg", "Invalid tags", "line", line, "err", err.Error())
	}

	glabels := gsplit(originalName)
	labels = mergeLabels(labels, glabels)

	mapping, mappingLabels, mappingPresent := c.mapper.GetMapping(parsedName, mapper.MetricTypeGauge)

	// add mapping labels to parsed labels
	for k, v := range mappingLabels {
		labels[k] = v
	}

	if (mappingPresent && mapping.Action == mapper.ActionTypeDrop) || (!mappingPresent && c.strictMatch) {
		return
	}

	var name string
	if mappingPresent {
		name = invalidMetricChars.ReplaceAllString(mapping.Name, "_")
	} else {
		name = invalidMetricChars.ReplaceAllString(parsedName, "_")
	}

	value, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		level.Info(c.logger).Log("msg", "Invalid value", "line", line)
		return
	}
	timestamp, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		level.Info(c.logger).Log("msg", "Invalid timestamp", "line", line)
		return
	}
	sample := graphiteSample{
		OriginalName: originalName,
		Name:         name,
		Value:        value,
		Labels:       labels,
		Type:         prometheus.GaugeValue,
		Help:         fmt.Sprintf("Graphite metric %s", name),
		Timestamp:    time.Unix(int64(timestamp), int64(math.Mod(timestamp, 1.0)*1e9)),
	}
	level.Debug(c.logger).Log("msg", "Processing sample", "sample", sample)
	c.lastProcessed.Set(float64(time.Now().UnixNano()) / 1e9)
	c.sampleCh <- &sample
}

// Collect implements prometheus.Collector.
func (c graphiteCollector) Collect(ch chan<- prometheus.Metric) {
	c.lastProcessed.Collect(ch)
	c.sampleExpiryMetric.Collect(ch)
	c.tagParseFailures.Collect(ch)

	max := cap(c.sampleCh)
	count := 0
	dedup := make(map[string]prometheus.Metric, len(c.sampleCh))

	func() {
		for {
			select {
			case sample := <-c.sampleCh:
				count++
				var metric prometheus.Metric
				metric = prometheus.MustNewConstMetric(
					prometheus.NewDesc(sample.Name, sample.Help, []string{}, sample.Labels),
					sample.Type,
					sample.Value,
				)
				if c.exposeTimestamps {
					metric = prometheus.NewMetricWithTimestamp(sample.Timestamp, metric)
				}
				dedup[metric.Desc().String()] = metric
				if count > max {
					return
				}
			default:
				return
			}
		}
	}()

	for _, metric := range dedup {
		ch <- metric
	}
}

// Describe implements prometheus.Collector but does not yield a description
// for Graphite metrics, allowing inconsistent label sets
func (c graphiteCollector) Describe(ch chan<- *prometheus.Desc) {
	c.lastProcessed.Describe(ch)
	c.sampleExpiryMetric.Describe(ch)
	c.tagParseFailures.Describe(ch)
}

type graphiteSample struct {
	OriginalName string
	Name         string
	Labels       prometheus.Labels
	Help         string
	Value        float64
	Type         prometheus.ValueType
	Timestamp    time.Time
}

func (s graphiteSample) String() string {
	return fmt.Sprintf("%#v", s)
}

type metricMapper interface {
	GetMapping(string, mapper.MetricType) (*mapper.MetricMapping, prometheus.Labels, bool)
	InitFromFile(string, int, ...mapper.CacheOption) error
	InitCache(int, ...mapper.CacheOption)
}
