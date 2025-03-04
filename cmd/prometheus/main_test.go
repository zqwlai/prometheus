// Copyright 2017 The Prometheus Authors
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

package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/notifier"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/rules"
)

var (
	promPath    = os.Args[0]
	promConfig  = filepath.Join("..", "..", "documentation", "examples", "prometheus.yml")
	agentConfig = filepath.Join("..", "..", "documentation", "examples", "prometheus-agent.yml")
	promData    = filepath.Join(os.TempDir(), "data")
)

func TestMain(m *testing.M) {
	for i, arg := range os.Args {
		if arg == "-test.main" {
			os.Args = append(os.Args[:i], os.Args[i+1:]...)
			main()
			return
		}
	}

	// On linux with a global proxy the tests will fail as the go client(http,grpc) tries to connect through the proxy.
	os.Setenv("no_proxy", "localhost,127.0.0.1,0.0.0.0,:")

	exitCode := m.Run()
	os.RemoveAll(promData)
	os.Exit(exitCode)
}

func TestComputeExternalURL(t *testing.T) {
	tests := []struct {
		input string
		valid bool
	}{
		{
			input: "",
			valid: true,
		},
		{
			input: "http://proxy.com/prometheus",
			valid: true,
		},
		{
			input: "'https://url/prometheus'",
			valid: false,
		},
		{
			input: "'relative/path/with/quotes'",
			valid: false,
		},
		{
			input: "http://alertmanager.company.com",
			valid: true,
		},
		{
			input: "https://double--dash.de",
			valid: true,
		},
		{
			input: "'http://starts/with/quote",
			valid: false,
		},
		{
			input: "ends/with/quote\"",
			valid: false,
		},
	}

	for _, test := range tests {
		_, err := computeExternalURL(test.input, "0.0.0.0:9090")
		if test.valid {
			require.NoError(t, err)
		} else {
			require.Error(t, err, "input=%q", test.input)
		}
	}
}

// Let's provide an invalid configuration file and verify the exit status indicates the error.
func TestFailedStartupExitCode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	fakeInputFile := "fake-input-file"
	expectedExitStatus := 2

	prom := exec.Command(promPath, "-test.main", "--config.file="+fakeInputFile)
	err := prom.Run()
	require.Error(t, err)

	if exitError, ok := err.(*exec.ExitError); ok {
		status := exitError.Sys().(syscall.WaitStatus)
		require.Equal(t, expectedExitStatus, status.ExitStatus())
	} else {
		t.Errorf("unable to retrieve the exit status for prometheus: %v", err)
	}
}

type senderFunc func(alerts ...*notifier.Alert)

func (s senderFunc) Send(alerts ...*notifier.Alert) {
	s(alerts...)
}

func TestSendAlerts(t *testing.T) {
	testCases := []struct {
		in  []*rules.Alert
		exp []*notifier.Alert
	}{
		{
			in: []*rules.Alert{
				{
					Labels:      []labels.Label{{Name: "l1", Value: "v1"}},
					Annotations: []labels.Label{{Name: "a2", Value: "v2"}},
					ActiveAt:    time.Unix(1, 0),
					FiredAt:     time.Unix(2, 0),
					ValidUntil:  time.Unix(3, 0),
				},
			},
			exp: []*notifier.Alert{
				{
					Labels:       []labels.Label{{Name: "l1", Value: "v1"}},
					Annotations:  []labels.Label{{Name: "a2", Value: "v2"}},
					StartsAt:     time.Unix(2, 0),
					EndsAt:       time.Unix(3, 0),
					GeneratorURL: "http://localhost:9090/graph?g0.expr=up&g0.tab=1",
				},
			},
		},
		{
			in: []*rules.Alert{
				{
					Labels:      []labels.Label{{Name: "l1", Value: "v1"}},
					Annotations: []labels.Label{{Name: "a2", Value: "v2"}},
					ActiveAt:    time.Unix(1, 0),
					FiredAt:     time.Unix(2, 0),
					ResolvedAt:  time.Unix(4, 0),
				},
			},
			exp: []*notifier.Alert{
				{
					Labels:       []labels.Label{{Name: "l1", Value: "v1"}},
					Annotations:  []labels.Label{{Name: "a2", Value: "v2"}},
					StartsAt:     time.Unix(2, 0),
					EndsAt:       time.Unix(4, 0),
					GeneratorURL: "http://localhost:9090/graph?g0.expr=up&g0.tab=1",
				},
			},
		},
		{
			in: []*rules.Alert{},
		},
	}

	for i, tc := range testCases {
		tc := tc
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			senderFunc := senderFunc(func(alerts ...*notifier.Alert) {
				if len(tc.in) == 0 {
					t.Fatalf("sender called with 0 alert")
				}
				require.Equal(t, tc.exp, alerts)
			})
			sendAlerts(senderFunc, "http://localhost:9090")(context.TODO(), "up", tc.in...)
		})
	}
}

func TestWALSegmentSizeBounds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	for size, expectedExitStatus := range map[string]int{"9MB": 1, "257MB": 1, "10": 2, "1GB": 1, "12MB": 0} {
		prom := exec.Command(promPath, "-test.main", "--storage.tsdb.wal-segment-size="+size, "--web.listen-address=0.0.0.0:0", "--config.file="+promConfig)

		// Log stderr in case of failure.
		stderr, err := prom.StderrPipe()
		require.NoError(t, err)
		go func() {
			slurp, _ := ioutil.ReadAll(stderr)
			t.Log(string(slurp))
		}()

		err = prom.Start()
		require.NoError(t, err)

		if expectedExitStatus == 0 {
			done := make(chan error, 1)
			go func() { done <- prom.Wait() }()
			select {
			case err := <-done:
				t.Errorf("prometheus should be still running: %v", err)
			case <-time.After(5 * time.Second):
				prom.Process.Kill()
				<-done
			}
			continue
		}

		err = prom.Wait()
		require.Error(t, err)
		if exitError, ok := err.(*exec.ExitError); ok {
			status := exitError.Sys().(syscall.WaitStatus)
			require.Equal(t, expectedExitStatus, status.ExitStatus())
		} else {
			t.Errorf("unable to retrieve the exit status for prometheus: %v", err)
		}
	}
}

func TestMaxBlockChunkSegmentSizeBounds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	for size, expectedExitStatus := range map[string]int{"512KB": 1, "1MB": 0} {
		prom := exec.Command(promPath, "-test.main", "--storage.tsdb.max-block-chunk-segment-size="+size, "--web.listen-address=0.0.0.0:0", "--config.file="+promConfig)

		// Log stderr in case of failure.
		stderr, err := prom.StderrPipe()
		require.NoError(t, err)
		go func() {
			slurp, _ := ioutil.ReadAll(stderr)
			t.Log(string(slurp))
		}()

		err = prom.Start()
		require.NoError(t, err)

		if expectedExitStatus == 0 {
			done := make(chan error, 1)
			go func() { done <- prom.Wait() }()
			select {
			case err := <-done:
				t.Errorf("prometheus should be still running: %v", err)
			case <-time.After(5 * time.Second):
				prom.Process.Kill()
				<-done
			}
			continue
		}

		err = prom.Wait()
		require.Error(t, err)
		if exitError, ok := err.(*exec.ExitError); ok {
			status := exitError.Sys().(syscall.WaitStatus)
			require.Equal(t, expectedExitStatus, status.ExitStatus())
		} else {
			t.Errorf("unable to retrieve the exit status for prometheus: %v", err)
		}
	}
}

func TestTimeMetrics(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "time_metrics_e2e")
	require.NoError(t, err)

	defer func() {
		require.NoError(t, os.RemoveAll(tmpDir))
	}()

	reg := prometheus.NewRegistry()
	db, err := openDBWithMetrics(tmpDir, log.NewNopLogger(), reg, nil, nil)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, db.Close())
	}()

	// Check initial values.
	require.Equal(t, map[string]float64{
		"prometheus_tsdb_lowest_timestamp_seconds": float64(math.MaxInt64) / 1000,
		"prometheus_tsdb_head_min_time_seconds":    float64(math.MaxInt64) / 1000,
		"prometheus_tsdb_head_max_time_seconds":    float64(math.MinInt64) / 1000,
	}, getCurrentGaugeValuesFor(t, reg,
		"prometheus_tsdb_lowest_timestamp_seconds",
		"prometheus_tsdb_head_min_time_seconds",
		"prometheus_tsdb_head_max_time_seconds",
	))

	app := db.Appender(context.Background())
	_, err = app.Append(0, labels.FromStrings(model.MetricNameLabel, "a"), 1000, 1)
	require.NoError(t, err)
	_, err = app.Append(0, labels.FromStrings(model.MetricNameLabel, "a"), 2000, 1)
	require.NoError(t, err)
	_, err = app.Append(0, labels.FromStrings(model.MetricNameLabel, "a"), 3000, 1)
	require.NoError(t, err)
	require.NoError(t, app.Commit())

	require.Equal(t, map[string]float64{
		"prometheus_tsdb_lowest_timestamp_seconds": 1.0,
		"prometheus_tsdb_head_min_time_seconds":    1.0,
		"prometheus_tsdb_head_max_time_seconds":    3.0,
	}, getCurrentGaugeValuesFor(t, reg,
		"prometheus_tsdb_lowest_timestamp_seconds",
		"prometheus_tsdb_head_min_time_seconds",
		"prometheus_tsdb_head_max_time_seconds",
	))
}

func getCurrentGaugeValuesFor(t *testing.T, reg prometheus.Gatherer, metricNames ...string) map[string]float64 {
	f, err := reg.Gather()
	require.NoError(t, err)

	res := make(map[string]float64, len(metricNames))
	for _, g := range f {
		for _, m := range metricNames {
			if g.GetName() != m {
				continue
			}

			require.Equal(t, 1, len(g.GetMetric()))
			if _, ok := res[m]; ok {
				t.Error("expected only one metric family for", m)
				t.FailNow()
			}
			res[m] = *g.GetMetric()[0].GetGauge().Value
		}
	}
	return res
}

func TestAgentSuccessfulStartup(t *testing.T) {
	prom := exec.Command(promPath, "-test.main", "--enable-feature=agent", "--config.file="+agentConfig)
	err := prom.Start()
	require.NoError(t, err)

	expectedExitStatus := 0
	actualExitStatus := 0

	done := make(chan error, 1)
	go func() { done <- prom.Wait() }()
	select {
	case err := <-done:
		t.Logf("prometheus agent should be still running: %v", err)
		actualExitStatus = prom.ProcessState.ExitCode()
	case <-time.After(5 * time.Second):
		prom.Process.Kill()
	}
	require.Equal(t, expectedExitStatus, actualExitStatus)
}

func TestAgentStartupWithInvalidConfig(t *testing.T) {
	prom := exec.Command(promPath, "-test.main", "--enable-feature=agent", "--config.file="+promConfig)
	err := prom.Start()
	require.NoError(t, err)

	expectedExitStatus := 2
	actualExitStatus := 0

	done := make(chan error, 1)
	go func() { done <- prom.Wait() }()
	select {
	case err := <-done:
		t.Logf("prometheus agent should not be running: %v", err)
		actualExitStatus = prom.ProcessState.ExitCode()
	case <-time.After(5 * time.Second):
		prom.Process.Kill()
	}
	require.Equal(t, expectedExitStatus, actualExitStatus)
}

func TestModeSpecificFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	testcases := []struct {
		mode       string
		arg        string
		exitStatus int
	}{
		{"agent", "--storage.agent.path", 0},
		{"server", "--storage.tsdb.path", 0},
		{"server", "--storage.agent.path", 3},
		{"agent", "--storage.tsdb.path", 3},
	}

	for _, tc := range testcases {
		t.Run(fmt.Sprintf("%s mode with option %s", tc.mode, tc.arg), func(t *testing.T) {
			args := []string{"-test.main", tc.arg, t.TempDir()}

			if tc.mode == "agent" {
				args = append(args, "--enable-feature=agent", "--config.file="+agentConfig)
			} else {
				args = append(args, "--config.file="+promConfig)
			}

			prom := exec.Command(promPath, args...)

			// Log stderr in case of failure.
			stderr, err := prom.StderrPipe()
			require.NoError(t, err)
			go func() {
				slurp, _ := ioutil.ReadAll(stderr)
				t.Log(string(slurp))
			}()

			err = prom.Start()
			require.NoError(t, err)

			if tc.exitStatus == 0 {
				done := make(chan error, 1)
				go func() { done <- prom.Wait() }()
				select {
				case err := <-done:
					t.Errorf("prometheus should be still running: %v", err)
				case <-time.After(5 * time.Second):
					prom.Process.Kill()
					<-done
				}
				return
			}

			err = prom.Wait()
			require.Error(t, err)
			if exitError, ok := err.(*exec.ExitError); ok {
				status := exitError.Sys().(syscall.WaitStatus)
				require.Equal(t, tc.exitStatus, status.ExitStatus())
			} else {
				t.Errorf("unable to retrieve the exit status for prometheus: %v", err)
			}
		})
	}
}
