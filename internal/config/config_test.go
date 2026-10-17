package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExamplesLoad(t *testing.T) {
	paths, _ := filepath.Glob("../../examples/*.yaml")
	more, _ := filepath.Glob("../../examples/policies/*.yaml")
	paths = append(paths, more...)
	if len(paths) == 0 {
		t.Fatal("no example configs found")
	}
	for _, p := range paths {
		if _, err := Load(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestHoltWintersPeriod(t *testing.T) {
	c := load(t, `
interval: 10s
services:
  - name: web
    template: {image: x}
    metrics: [{type: cpu, target: 60}]
    model: {kind: holt_winters, season_length: 24h}
`)
	if got := c.Services[0].Model.Period; got != 8640 {
		t.Fatalf("period = %d, want 8640", got)
	}
	if c.Services[0].Behavior.ScaleDownStabilization != 5*time.Minute {
		t.Fatal("defaults not applied")
	}
}

func TestValidation(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"queue without source": {`
services:
  - name: w
    template: {image: x}
    metrics: [{type: queue_depth, target: 10}]`, "needs queue.url"},
		"rps without prometheus": {`
services:
  - name: w
    template: {image: x}
    metrics: [{type: rps, target: 10}]`, "needs prometheus"},
		"unknown field": {`
services:
  - name: w
    template: {image: x, imagee: y}
    metrics: [{type: cpu, target: 10}]`, "imagee"},
		"season too short": {`
services:
  - name: w
    template: {image: x}
    metrics: [{type: cpu, target: 10}]
    model: {kind: holt_winters, season_length: 5s}`, "season_length"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadErr(t, tc.yaml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func load(t *testing.T, y string) *Config {
	c, err := loadErr(t, y)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func loadErr(t *testing.T, y string) (*Config, error) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}
