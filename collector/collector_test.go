package collector

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mattmendick/ecobee-exporter/internal/ecobee"
	"github.com/prometheus/client_golang/prometheus"
)

type fakeAPI struct {
	revisions          map[string]string
	data               []ecobee.Thermostat
	err, detailErr     error
	summaries, fetches int
}

func (f *fakeAPI) Summary(context.Context) (map[string]string, error) {
	f.summaries++
	return f.revisions, f.err
}
func (f *fakeAPI) Thermostats(context.Context) ([]ecobee.Thermostat, error) {
	f.fetches++
	return f.data, f.detailErr
}

func fixture(t *testing.T, input string) []ecobee.Thermostat {
	t.Helper()
	var data []ecobee.Thermostat
	if err := json.Unmarshal([]byte(input), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// Gather through a pedantic registry to catch descriptor/label inconsistencies.
func values(t *testing.T, c *Collector) map[string][]float64 {
	t.Helper()
	r := prometheus.NewPedanticRegistry()
	r.MustRegister(c)
	families, err := r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]float64)
	for _, family := range families {
		for _, metric := range family.Metric {
			if metric.Gauge != nil {
				out[family.GetName()] = append(out[family.GetName()], metric.Gauge.GetValue())
			}
			if metric.Counter != nil {
				out[family.GetName()] = append(out[family.GetName()], metric.Counter.GetValue())
			}
		}
	}
	return out
}

func TestCachingAndFailureRecovery(t *testing.T) {
	api := &fakeAPI{revisions: map[string]string{"test": "rev1"}, data: fixture(t, `[{"identifier":"test","name":"Test","runtime":{"connected":true,"actualTemperature":688}}]`)}
	c := New(api, 3*time.Minute, 15*time.Minute)
	if got := values(t, c)["ecobee_up"][0]; got != 0 {
		t.Fatal(got)
	}
	if err := c.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		values(t, c)
	}
	if api.summaries != 1 || api.fetches != 1 {
		t.Fatal("scrapes called the API")
	}
	if err := c.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.fetches != 1 {
		t.Fatal("unchanged revision refetched details")
	}
	api.revisions = map[string]string{"test": "rev2"}
	api.detailErr = errors.New("details failed")
	if err := c.poll(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	v := values(t, c)
	if v["ecobee_up"][0] != 0 || v["ecobee_poll_errors_total"][0] != 1 || v["ecobee_actual_temperature"][0] != 68.8 {
		t.Fatal(v)
	}
	api.detailErr = nil
	if err := c.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.fetches != 3 || values(t, c)["ecobee_up"][0] != 1 {
		t.Fatal("failed fetch committed revisions or failed to recover")
	}
	api.err = errors.New("offline")
	if err := c.poll(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	c.lastSuccess = time.Now().Add(-16 * time.Minute)
	v = values(t, c)
	if _, ok := v["ecobee_actual_temperature"]; ok {
		t.Fatal("expired telemetry retained")
	}
	if _, ok := v["ecobee_last_successful_poll_timestamp_seconds"]; !ok {
		t.Fatal("health timestamp missing")
	}
}

func TestLegacyMetricsAndInvalidData(t *testing.T) {
	api := &fakeAPI{revisions: map[string]string{"test": "rev1"}, data: fixture(t, `[{"identifier":"test","name":"Test","settings":{"hvacMode":"heat"},"runtime":{"connected":true,"actualTemperature":688,"desiredHeat":690,"desiredCool":760},"extendedRuntime":{"fan":[0,100,300],"auxHeat1":[0,0,250],"heatPump1":[],"cool1":[1],"cool2":[0,0,-5002],"hvacMode":["heatOff","heatOff","heatStage1On"]},"remoteSensors":[{"id":"sensor","name":"Room","type":"remote","inUse":true,"capability":[{"type":"temperature","value":"679"},{"type":"humidity","value":"55"},{"type":"occupancy","value":"true"}]}]}]`)}
	c := New(api, 3*time.Minute, 15*time.Minute)
	if err := c.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := values(t, c)
	for name, want := range map[string]float64{"actual_temperature": 68.8, "target_temperature_min": 69, "target_temperature_max": 76, "temperature": 67.9, "humidity": 55, "occupancy": 1, "in_use": 1, "currenthvacmode": 1, "last_interval_energized_state": 1} {
		if got := v["ecobee_"+name]; len(got) != 1 || got[0] != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	if len(v["ecobee_last_interval_runtime"]) != 2 {
		t.Fatal("short, missing or invalid runtime arrays should be omitted", v)
	}
	// All legacy label names are preserved, including stage_name/stage_mechanism.
	r := prometheus.NewPedanticRegistry()
	r.MustRegister(c)
	families, err := r.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "ecobee_last_interval_runtime" {
			for _, metric := range family.Metric {
				labels := map[string]string{}
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["thermostat_id"] != "test" || labels["stage_name"] == "" || labels["stage_mechanism"] == "" {
					t.Fatal(labels)
				}
			}
		}
	}
}

func TestMissingSentinelAndDisconnectedValues(t *testing.T) {
	for _, sensorValue := range []string{"unknown", "-5002", "-5003", "NaN", "+Inf", "garbage"} {
		t.Run(sensorValue, func(t *testing.T) {
			data := fixture(t, `[{"identifier":"test","runtime":{"connected":true,"actualTemperature":-5002,"desiredHeat":-5003},"remoteSensors":[{"id":"s","capability":[{"type":"temperature","value":"`+sensorValue+`"}]}]}]`)
			api := &fakeAPI{revisions: map[string]string{"test": "rev"}, data: data}
			c := New(api, 3*time.Minute, 15*time.Minute)
			if err := c.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			for name := range values(t, c) {
				if name == "ecobee_temperature" || name == "ecobee_actual_temperature" || name == "ecobee_target_temperature_min" || name == "ecobee_target_temperature_max" {
					t.Fatal("invalid or missing value exported", name)
				}
			}
			c.thermostats[0].Runtime.Connected = false
			v := values(t, c)
			if v["ecobee_connected"][0] != 0 {
				t.Fatal(v)
			}
			if _, ok := v["ecobee_in_use"]; ok {
				t.Fatal("disconnected sensor data exported")
			}
		})
	}
}

func TestConcurrentScrapes(t *testing.T) {
	api := &fakeAPI{revisions: map[string]string{"test": "rev"}, data: fixture(t, `[{"identifier":"test","runtime":{"connected":true,"actualTemperature":700}}]`)}
	c := New(api, 3*time.Minute, 15*time.Minute)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				values(t, c)
			}
		})
	}
	for range 20 {
		if err := c.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

type blockingAPI struct{ started chan struct{} }

func (b blockingAPI) Summary(ctx context.Context) (map[string]string, error) {
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (b blockingAPI) Thermostats(context.Context) ([]ecobee.Thermostat, error) {
	panic("unexpected details call")
}

func TestSlowAPIDoesNotBlockScrapesAndCancellation(t *testing.T) {
	api := blockingAPI{make(chan struct{})}
	c := New(api, 3*time.Minute, 15*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	<-api.started
	values(t, c)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("poller did not cancel")
	}
}
