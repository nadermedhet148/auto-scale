// Package config loads and validates the dockscale YAML configuration.
package config

import (
	"bytes"
	"fmt"
	"os"
	"time"

	units "github.com/docker/go-units"
	"gopkg.in/yaml.v3"
)

// Supported scaling signals.
const (
	MetricCPU        = "cpu"         // % of the container's CPU limit (or of one core when unlimited)
	MetricMemory     = "memory"      // % of the container's memory limit
	MetricRPS        = "rps"         // requests/second per replica, from Traefik
	MetricLatencyP95 = "latency_p95" // p95 latency in milliseconds, from Traefik
	MetricQueueDepth = "queue_depth" // pending messages per replica, from Redis or RabbitMQ
)

// AllMetrics lists every metric type, in the order recordings use.
var AllMetrics = []string{MetricCPU, MetricMemory, MetricRPS, MetricLatencyP95, MetricQueueDepth}

// Supported queue backends.
const (
	QueueRedisList = "redis_list"
	QueueRabbitMQ  = "rabbitmq"
)

// Supported estimators.
const (
	ModelRaw  = "raw"
	ModelEWMA = "ewma"
	ModelHolt = "holt"
	ModelHW   = "holt_winters"
)

type Config struct {
	Interval time.Duration `yaml:"interval"`
	Listen   string        `yaml:"listen"`
	DryRun   bool          `yaml:"dry_run"`
	Services []Service     `yaml:"services"`
}

type Service struct {
	Name       string      `yaml:"name"`
	Min        int         `yaml:"min"`
	Max        int         `yaml:"max"`
	Template   Template    `yaml:"template"`
	Metrics    []Metric    `yaml:"metrics"`
	Prometheus *Prometheus `yaml:"prometheus"`
	Queue      *Queue      `yaml:"queue"`
	Behavior   Behavior    `yaml:"behavior"`
	Model      Model       `yaml:"model"`
}

// Template describes how to create one replica.
type Template struct {
	Image       string            `yaml:"image"`
	Cmd         []string          `yaml:"cmd"`
	Env         []string          `yaml:"env"`
	Labels      map[string]string `yaml:"labels"`
	Network     string            `yaml:"network"`
	CPUs        float64           `yaml:"cpus"`
	Memory      string            `yaml:"memory"`
	StopTimeout time.Duration     `yaml:"stop_timeout"`

	MemoryBytes int64 `yaml:"-"`
}

type Metric struct {
	Type   string  `yaml:"type"`
	Target float64 `yaml:"target"`
}

// Prometheus points at Traefik's metrics endpoint.
type Prometheus struct {
	URL     string `yaml:"url"`
	Service string `yaml:"service"` // Traefik service label, e.g. "web@docker"
}

// Queue points at the backlog a worker service drains.
type Queue struct {
	Kind  string `yaml:"kind"`  // redis_list | rabbitmq
	URL   string `yaml:"url"`   // redis://[:pass@]host:6379/0 or http://user:pass@host:15672
	Name  string `yaml:"name"`  // Redis list key or RabbitMQ queue name
	VHost string `yaml:"vhost"` // RabbitMQ only, default "/"
}

type Behavior struct {
	Tolerance              float64       `yaml:"tolerance"`
	ScaleUpCooldown        time.Duration `yaml:"scale_up_cooldown"`
	ScaleDownCooldown      time.Duration `yaml:"scale_down_cooldown"`
	ScaleDownStabilization time.Duration `yaml:"scale_down_stabilization"`
	MaxStepUp              int           `yaml:"max_step_up"`
	MaxStepDown            int           `yaml:"max_step_down"`
}

type Model struct {
	Kind          string        `yaml:"kind"`
	Alpha         float64       `yaml:"alpha"`
	Beta          float64       `yaml:"beta"`
	Gamma         float64       `yaml:"gamma"`         // holt_winters seasonal smoothing
	SeasonLength  time.Duration `yaml:"season_length"` // holt_winters cycle, e.g. 24h
	Horizon       int           `yaml:"horizon"`
	OutlierK      float64       `yaml:"outlier_k"` // negative disables the outlier filter
	OutlierWindow int           `yaml:"outlier_window"`

	Period int `yaml:"-"` // SeasonLength in intervals
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Interval == 0 {
		c.Interval = 10 * time.Second
	}
	if c.Listen == "" {
		c.Listen = ":9090"
	}
	for i := range c.Services {
		s := &c.Services[i]
		if s.Min == 0 {
			s.Min = 1
		}
		if s.Max == 0 {
			s.Max = 10
		}
		if s.Template.StopTimeout == 0 {
			s.Template.StopTimeout = 10 * time.Second
		}

		b := &s.Behavior
		if b.Tolerance == 0 {
			b.Tolerance = 0.1
		}
		if b.ScaleUpCooldown == 0 {
			b.ScaleUpCooldown = 30 * time.Second
		}
		if b.ScaleDownCooldown == 0 {
			b.ScaleDownCooldown = 2 * time.Minute
		}
		if b.ScaleDownStabilization == 0 {
			b.ScaleDownStabilization = 5 * time.Minute
		}
		if b.MaxStepUp == 0 {
			b.MaxStepUp = 4
		}
		if b.MaxStepDown == 0 {
			b.MaxStepDown = 1
		}

		m := &s.Model
		if m.Kind == "" {
			m.Kind = ModelEWMA
		}
		if m.Alpha == 0 {
			m.Alpha = 0.5
		}
		if m.Beta == 0 {
			m.Beta = 0.3
		}
		if m.Gamma == 0 {
			m.Gamma = 0.1
		}
		if s.Queue != nil && s.Queue.VHost == "" {
			s.Queue.VHost = "/"
		}
		if m.Horizon == 0 {
			m.Horizon = 3
		}
		if m.OutlierK == 0 {
			m.OutlierK = 3.5
		}
		if m.OutlierWindow == 0 {
			m.OutlierWindow = 6
		}
	}
}

func (c *Config) validate() error {
	if len(c.Services) == 0 {
		return fmt.Errorf("no services configured")
	}
	seen := map[string]bool{}
	for i := range c.Services {
		s := &c.Services[i]
		if s.Model.Kind == ModelHW {
			s.Model.Period = int(s.Model.SeasonLength / c.Interval)
			if s.Model.Period < 2 {
				return fmt.Errorf("service %q: model.season_length must be at least 2 intervals", s.Name)
			}
		}
		if err := s.validate(); err != nil {
			return fmt.Errorf("service %q: %w", s.Name, err)
		}
		if seen[s.Name] {
			return fmt.Errorf("service %q: duplicate name", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

func (s *Service) validate() error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if s.Template.Image == "" {
		return fmt.Errorf("template.image is required")
	}
	if s.Min < 1 {
		return fmt.Errorf("min must be >= 1")
	}
	if s.Max < s.Min {
		return fmt.Errorf("max (%d) must be >= min (%d)", s.Max, s.Min)
	}
	if s.Template.Memory != "" {
		n, err := units.RAMInBytes(s.Template.Memory)
		if err != nil {
			return fmt.Errorf("template.memory: %w", err)
		}
		s.Template.MemoryBytes = n
	}
	if len(s.Metrics) == 0 {
		return fmt.Errorf("at least one metric is required")
	}
	for _, m := range s.Metrics {
		switch m.Type {
		case MetricCPU, MetricMemory:
		case MetricRPS, MetricLatencyP95:
			if s.Prometheus == nil || s.Prometheus.URL == "" || s.Prometheus.Service == "" {
				return fmt.Errorf("metric %q needs prometheus.url and prometheus.service", m.Type)
			}
		case MetricQueueDepth:
			if s.Queue == nil || s.Queue.URL == "" || s.Queue.Name == "" {
				return fmt.Errorf("metric %q needs queue.url and queue.name", m.Type)
			}
			if s.Queue.Kind != QueueRedisList && s.Queue.Kind != QueueRabbitMQ {
				return fmt.Errorf("queue.kind must be %q or %q", QueueRedisList, QueueRabbitMQ)
			}
		default:
			return fmt.Errorf("unknown metric type %q", m.Type)
		}
		if m.Target <= 0 {
			return fmt.Errorf("metric %q: target must be > 0", m.Type)
		}
	}
	switch s.Model.Kind {
	case ModelRaw, ModelEWMA, ModelHolt, ModelHW:
	default:
		return fmt.Errorf("unknown model.kind %q", s.Model.Kind)
	}
	if s.Model.Alpha <= 0 || s.Model.Alpha > 1 || s.Model.Beta <= 0 || s.Model.Beta > 1 || s.Model.Gamma <= 0 || s.Model.Gamma > 1 {
		return fmt.Errorf("model.alpha, model.beta and model.gamma must be in (0, 1]")
	}
	if s.Behavior.Tolerance < 0 {
		return fmt.Errorf("behavior.tolerance must be >= 0")
	}
	return nil
}

// UsesDockerStats reports whether any metric is read from the Docker stats API.
func (s *Service) UsesDockerStats() bool {
	for _, m := range s.Metrics {
		if m.Type == MetricCPU || m.Type == MetricMemory {
			return true
		}
	}
	return false
}

// UsesTraefik reports whether any metric is scraped from Traefik.
func (s *Service) UsesTraefik() bool {
	for _, m := range s.Metrics {
		if m.Type == MetricRPS || m.Type == MetricLatencyP95 {
			return true
		}
	}
	return false
}

// UsesQueue reports whether any metric reads a queue backlog.
func (s *Service) UsesQueue() bool {
	for _, m := range s.Metrics {
		if m.Type == MetricQueueDepth {
			return true
		}
	}
	return false
}
