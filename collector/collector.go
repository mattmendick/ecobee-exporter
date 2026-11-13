// Package collector exposes cached ecobee telemetry as Prometheus metrics.
package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/mattmendick/ecobee-exporter/internal/ecobee"
	"github.com/prometheus/client_golang/prometheus"
)

type API interface {
	Summary(context.Context) (map[string]string, error)
	Thermostats(context.Context) ([]ecobee.Thermostat, error)
}

type Collector struct {
	client                 API
	interval, maxAge       time.Duration
	mu                     sync.RWMutex
	revisions              map[string]string
	thermostats            []ecobee.Thermostat
	lastSuccess, lastFetch time.Time
	duration               float64
	success                bool
	errors                 uint64
	desc                   map[string]*prometheus.Desc
}

func New(client API, interval, maxAge time.Duration) *Collector {
	c := &Collector{client: client, interval: interval, maxAge: maxAge, desc: make(map[string]*prometheus.Desc)}
	t := []string{"thermostat_id", "thermostat_name"}
	s := []string{"thermostat_id", "thermostat_name", "sensor_id", "sensor_name", "sensor_type"}
	add := func(name, help string, labels []string) {
		c.desc[name] = prometheus.NewDesc("ecobee_"+name, help, labels, nil)
	}
	add("fetch_time", "Duration of the last API polling cycle in seconds.", nil)
	add("up", "Whether the last API polling cycle succeeded (not Prometheus scrape health).", nil)
	add("poll_errors_total", "Number of failed API polling cycles.", nil)
	add("last_successful_poll_timestamp_seconds", "Unix time of the last successful API check, including unchanged summaries.", nil)
	add("last_data_fetch_timestamp_seconds", "Unix time of the last successful full thermostat fetch; not the device observation time.", nil)
	add("connected", "Whether the thermostat is connected to ecobee.", t)
	add("actual_temperature", "Thermostat-averaged current temperature in degrees Fahrenheit.", t)
	add("target_temperature_min", "Heating target in degrees Fahrenheit.", t)
	add("target_temperature_max", "Cooling target in degrees Fahrenheit.", t)
	add("temperature", "Sensor temperature in degrees Fahrenheit.", s)
	add("humidity", "Sensor relative humidity in percent.", s)
	add("occupancy", "Computed occupancy: motion detected within the past 30 minutes (0 or 1).", s)
	add("in_use", "Whether a sensor participates in thermostat calculations (0 or 1).", s)
	add("currenthvacmode", "Configured HVAC mode; the selected mode has value 1.", append(append([]string{}, t...), "current_hvac_mode"))
	add("last_interval_runtime", "Runtime in the latest reported five-minute interval in seconds (0-300); not a counter.", append(append([]string{}, t...), "stage_name", "stage_mechanism"))
	add("last_interval_energized_state", "HVAC state in the latest reported interval; the selected state has value 1.", append(append([]string{}, t...), "stage"))
	return c
}

// Run is the sole API caller. Scrapes never initiate network or authentication work.
func (c *Collector) Run(ctx context.Context) {
	delay := time.Duration(0)
	backoff := c.interval
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		err := c.poll(ctx)
		delay = c.interval
		if err != nil {
			slog.Error("ecobee poll failed", "error", err)
			delay = backoff
			backoff = min(backoff*2, max(30*time.Minute, c.interval))
			var rateLimit *ecobee.RateLimitError
			if errors.As(err, &rateLimit) {
				delay = max(delay, rateLimit.After)
			}
		} else {
			backoff = c.interval
		}
	}
}

func (c *Collector) poll(ctx context.Context) (err error) {
	start := time.Now()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.duration = time.Since(start).Seconds()
		c.success = err == nil
		if err != nil {
			c.errors++
		} else {
			c.lastSuccess = time.Now()
		}
	}()
	revisions, err := c.client.Summary(ctx)
	if err != nil {
		return err
	}
	if c.revisions != nil && maps.Equal(revisions, c.revisions) {
		return nil
	}
	thermostats, err := c.client.Thermostats(ctx)
	if err != nil {
		return err
	}
	if len(thermostats) != len(revisions) {
		return fmt.Errorf("thermostat list does not match summary; retry next cycle")
	}
	seen := make(map[string]bool, len(thermostats))
	for _, thermostat := range thermostats {
		if _, ok := revisions[thermostat.Identifier]; !ok || seen[thermostat.Identifier] {
			return fmt.Errorf("unexpected or duplicate thermostat identifier")
		}
		seen[thermostat.Identifier] = true
	}
	// Only commit revisions after details succeed, so a failed fetch is retried.
	c.mu.Lock()
	c.thermostats = thermostats
	c.lastFetch = time.Now()
	c.revisions = revisions
	c.mu.Unlock()
	return nil
}

func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range c.desc {
		ch <- desc
	}
}

func boolValue(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
func validNumber(v float64) bool {
	return v != -5002 && v != -5003 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	emit := func(name string, value float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(c.desc[name], prometheus.GaugeValue, value, labels...)
	}
	emit("fetch_time", c.duration)
	emit("up", boolValue(c.success))
	ch <- prometheus.MustNewConstMetric(c.desc["poll_errors_total"], prometheus.CounterValue, float64(c.errors))
	if c.lastSuccess.IsZero() {
		return
	}
	emit("last_successful_poll_timestamp_seconds", float64(c.lastSuccess.Unix()))
	emit("last_data_fetch_timestamp_seconds", float64(c.lastFetch.Unix()))
	// Retain last-known-good samples briefly, then omit them rather than report stale data forever.
	if time.Since(c.lastSuccess) > c.maxAge {
		return
	}
	for _, t := range c.thermostats {
		labels := []string{t.Identifier, t.Name}
		emit("connected", boolValue(t.Runtime.Connected), labels...)
		if !t.Runtime.Connected {
			continue
		}
		for name, value := range map[string]*int{"actual_temperature": t.Runtime.ActualTemperature, "target_temperature_min": t.Runtime.DesiredHeat, "target_temperature_max": t.Runtime.DesiredCool} {
			if value != nil && validNumber(float64(*value)) {
				emit(name, float64(*value)/10, labels...)
			}
		}
		if t.Settings.HvacMode != "" {
			emit("currenthvacmode", 1, t.Identifier, t.Name, t.Settings.HvacMode)
		}
		r := t.ExtendedRuntime
		for _, stage := range []struct {
			name, mechanism string
			values          []int
		}{
			{"heatStage1On", "heatPump1", r.HeatPump1}, {"heatStage2On", "heatPump2", r.HeatPump2},
			{"heatStage1On", "auxHeat1", r.AuxHeat1}, {"heatStage2On", "auxHeat2", r.AuxHeat2},
			{"compressorCoolStage1On", "cool1", r.Cool1}, {"compressorCoolStage2On", "cool2", r.Cool2},
			{"fan", "fan", r.Fan}, {"humidifier", "humidifier", r.Humidifier}, {"dehumidifier", "dehumidifier", r.Dehumidifier},
			{"economizer", "economizer", r.Economizer}, {"ventilator", "ventilator", r.Ventilator},
		} {
			// Only the third entry represents the expected latest interval. Never substitute an older entry.
			if len(stage.values) >= 3 && stage.values[2] >= 0 && stage.values[2] <= 300 {
				emit("last_interval_runtime", float64(stage.values[2]), t.Identifier, t.Name, stage.name, stage.mechanism)
			}
		}
		if len(r.HvacMode) >= 3 && r.HvacMode[2] != "" {
			emit("last_interval_energized_state", 1, t.Identifier, t.Name, r.HvacMode[2])
		}
		for _, sensor := range t.RemoteSensors {
			labels := []string{t.Identifier, t.Name, sensor.ID, sensor.Name, sensor.Type}
			emit("in_use", boolValue(sensor.InUse), labels...)
			for _, capability := range sensor.Capability {
				switch capability.Type {
				case "temperature", "humidity":
					v, err := strconv.ParseFloat(capability.Value, 64)
					if err != nil || !validNumber(v) {
						continue
					}
					if capability.Type == "temperature" {
						v /= 10
					} else if v < 0 || v > 100 {
						continue
					}
					emit(capability.Type, v, labels...)
				case "occupancy":
					if capability.Value == "true" || capability.Value == "false" {
						emit("occupancy", boolValue(capability.Value == "true"), labels...)
					}
				}
			}
		}
	}
}
