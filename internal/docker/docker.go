// Package docker wraps the Docker Engine API: listing, creating and removing
// a service's replicas, and sampling their CPU and memory usage.
package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"dockscale/internal/config"
)

// Labels put on every container dockscale creates. Only containers carrying
// them are ever counted or removed.
const (
	LabelManaged = "dockscale.managed"
	LabelService = "dockscale.service"
)

type Client struct {
	cli *client.Client

	mu   sync.Mutex
	prev map[string]cpuSample // last CPU counters per container, for deltas
}

type Replica struct {
	ID      string
	Name    string
	Created int64
}

type cpuSample struct {
	total, system uint64
}

// New connects using the DOCKER_HOST environment (or the platform default);
// opts are applied after it, e.g. client.WithHost in tests.
func New(opts ...client.Opt) (*Client, error) {
	cli, err := client.New(append([]client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}, opts...)...)
	if err != nil {
		return nil, err
	}
	return &Client{cli: cli, prev: map[string]cpuSample{}}, nil
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
	return err
}

// Replicas returns the service's live replicas, oldest first, and any
// stopped ones that should be cleaned up.
func (c *Client) Replicas(ctx context.Context, service string) (live, dead []Replica, err error) {
	list, err := c.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelManaged+"=true", LabelService+"="+service),
	})
	if err != nil {
		return nil, nil, err
	}
	for _, ct := range list.Items {
		r := Replica{ID: ct.ID, Created: ct.Created}
		if len(ct.Names) > 0 {
			r.Name = strings.TrimPrefix(ct.Names[0], "/")
		}
		switch ct.State {
		case container.StateRunning, container.StateRestarting, container.StatePaused:
			live = append(live, r)
		default:
			dead = append(dead, r)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Created < live[j].Created })
	return live, dead, nil
}

// Start creates and starts one replica from the service template, pulling
// the image first if it is missing.
func (c *Client) Start(ctx context.Context, svc config.Service) (string, error) {
	t := svc.Template
	labels := make(map[string]string, len(t.Labels)+2)
	for k, v := range t.Labels {
		labels[k] = v
	}
	labels[LabelManaged] = "true"
	labels[LabelService] = svc.Name

	cfg := &container.Config{Image: t.Image, Cmd: t.Cmd, Env: t.Env, Labels: labels}
	host := &container.HostConfig{RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}}
	host.NanoCPUs = int64(t.CPUs * 1e9)
	host.Memory = t.MemoryBytes
	var netCfg *network.NetworkingConfig
	if t.Network != "" {
		host.NetworkMode = container.NetworkMode(t.Network)
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{t.Network: {}},
		}
	}

	opts := client.ContainerCreateOptions{
		Config: cfg, HostConfig: host, NetworkingConfig: netCfg,
		Name: svc.Name + "-" + randSuffix(),
	}
	name := opts.Name
	resp, err := c.cli.ContainerCreate(ctx, opts)
	if errdefs.IsNotFound(err) {
		if perr := c.pull(ctx, t.Image); perr != nil {
			return "", perr
		}
		resp, err = c.cli.ContainerCreate(ctx, opts)
	}
	if err != nil {
		return "", fmt.Errorf("create %s: %w", name, err)
	}
	if _, err := c.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = c.cli.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true})
		return "", fmt.Errorf("start %s: %w", name, err)
	}
	return resp.ID, nil
}

// Remove stops a replica (SIGTERM, then SIGKILL after grace) and deletes it.
func (c *Client) Remove(ctx context.Context, id string, grace time.Duration) error {
	c.mu.Lock()
	delete(c.prev, id)
	c.mu.Unlock()

	secs := int(grace.Seconds())
	if _, err := c.cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &secs}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stop %s: %w", short(id), err)
	}
	if _, err := c.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove %s: %w", short(id), err)
	}
	return nil
}

func (c *Client) pull(ctx context.Context, ref string) error {
	resp, err := c.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	return nil
}

// Usage is the average resource usage across a set of replicas. CPU needs two
// samples per container, so CPUN is 0 on a container's first tick.
type Usage struct {
	CPU, Mem   float64
	CPUN, MemN int
	Errors     int
}

// Usage samples all containers in parallel. cpuLimit is the per-replica CPU
// limit in cores; CPU is reported as a percentage of it (of one core if 0).
func (c *Client) Usage(ctx context.Context, ids []string, cpuLimit float64) Usage {
	type result struct {
		cpu, mem     float64
		cpuOK, memOK bool
		err          error
	}
	results := make([]result, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			r := &results[i]
			r.cpu, r.cpuOK, r.mem, r.memOK, r.err = c.sample(ctx, id, cpuLimit)
		}(i, id)
	}
	wg.Wait()

	var u Usage
	for _, r := range results {
		if r.err != nil {
			u.Errors++
			continue
		}
		if r.cpuOK {
			u.CPU += r.cpu
			u.CPUN++
		}
		if r.memOK {
			u.Mem += r.mem
			u.MemN++
		}
	}
	if u.CPUN > 0 {
		u.CPU /= float64(u.CPUN)
	}
	if u.MemN > 0 {
		u.Mem /= float64(u.MemN)
	}
	return u
}

// statsJSON holds the fields of the Engine stats response we use.
type statsJSON struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
}

func (c *Client) sample(ctx context.Context, id string, cpuLimit float64) (cpu float64, cpuOK bool, mem float64, memOK bool, err error) {
	resp, err := c.cli.ContainerStats(ctx, id, client.ContainerStatsOptions{})
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var s statsJSON
	if err = json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return
	}

	// Same as `docker stats`: exclude reclaimable page cache from usage.
	if lim := s.MemoryStats.Limit; lim > 0 {
		used := s.MemoryStats.Usage
		inactive, ok := s.MemoryStats.Stats["inactive_file"] // cgroup v2
		if !ok {
			inactive = s.MemoryStats.Stats["total_inactive_file"] // cgroup v1
		}
		if inactive < used {
			used -= inactive
		}
		mem, memOK = float64(used)/float64(lim)*100, true
	}

	// One-shot stats carry no previous sample, so keep our own.
	cur := cpuSample{total: s.CPUStats.CPUUsage.TotalUsage, system: s.CPUStats.SystemUsage}
	c.mu.Lock()
	prev, had := c.prev[id]
	c.prev[id] = cur
	c.mu.Unlock()
	if had && cur.system > prev.system && cur.total >= prev.total {
		online := float64(s.CPUStats.OnlineCPUs)
		if online == 0 {
			online = 1
		}
		cores := float64(cur.total-prev.total) / float64(cur.system-prev.system) * online
		if cpuLimit > 0 {
			cores /= cpuLimit
		}
		cpu, cpuOK = cores*100, true
	}
	return
}

func randSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
