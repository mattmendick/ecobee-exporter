package main

import (
	"flag"
	"os"
	"strings"
	"testing"
)

func TestConfiguration(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"version-without-key", []string{"--version"}, ""},
		{"missing-key", nil, "set ECOBEE_APPKEY"},
		{"too-frequent", []string{"--appkey=test", "--poll-interval=60s"}, "poll-interval"},
		{"invalid-interval", []string{"--appkey=test", "--poll-interval=invalid"}, "poll-interval"},
		{"short-cache", []string{"--appkey=test", "--max-cache-age=1m"}, "max-cache-age"},
		{"unbounded-timeout", []string{"--appkey=test", "--api-timeout=0"}, "api-timeout"},
		{"long-timeout", []string{"--appkey=test", "--api-timeout=10m"}, "api-timeout"},
		{"positional", []string{"unexpected"}, "positional"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("ECOBEE_APPKEY", "")
			t.Setenv("ECOBEE_POLL_INTERVAL", "")
			t.Setenv("ECOBEE_MAX_CACHE_AGE", "")
			t.Setenv("ECOBEE_API_TIMEOUT", "")
			originalFlags, originalArgs := flag.CommandLine, os.Args
			defer func() { flag.CommandLine, os.Args = originalFlags, originalArgs }()
			flag.CommandLine = flag.NewFlagSet("test", flag.ContinueOnError)
			os.Args = append([]string{"test"}, test.args...)
			err := run()
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want %s", err, test.want)
			}
		})
	}
}
