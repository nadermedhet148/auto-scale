// Package queue reads the backlog of a work queue (Redis list or RabbitMQ
// queue) so worker services can scale on pending messages.
package queue

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"dockscale/internal/config"
)

// Source returns the number of messages waiting in a queue.
type Source interface {
	Depth(ctx context.Context) (float64, error)
}

func New(q config.Queue) (Source, error) {
	switch q.Kind {
	case config.QueueRedisList:
		u, err := url.Parse(q.URL)
		if err != nil || u.Scheme != "redis" {
			return nil, fmt.Errorf("queue.url must look like redis://[:password@]host:6379/0")
		}
		return &Redis{url: u, key: q.Name}, nil
	case config.QueueRabbitMQ:
		u, err := url.Parse(q.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("queue.url must be the RabbitMQ management API, e.g. http://guest:guest@rabbitmq:15672")
		}
		return &RabbitMQ{base: u, vhost: q.VHost, name: q.Name, client: &http.Client{Timeout: 5 * time.Second}}, nil
	}
	return nil, fmt.Errorf("unknown queue kind %q", q.Kind)
}

// Redis reads LLEN of a list over a short-lived connection, speaking just
// enough RESP for AUTH, SELECT and LLEN.
type Redis struct {
	url *url.URL
	key string
}

func (r *Redis) Depth(ctx context.Context) (float64, error) {
	host := r.url.Host
	if r.url.Port() == "" {
		host = net.JoinHostPort(r.url.Hostname(), "6379")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	rd := bufio.NewReader(conn)

	if pass, ok := r.url.User.Password(); ok {
		args := []string{"AUTH", pass}
		if user := r.url.User.Username(); user != "" {
			args = []string{"AUTH", user, pass}
		}
		if _, err := command(conn, rd, args...); err != nil {
			return 0, err
		}
	}
	if db := strings.TrimPrefix(r.url.Path, "/"); db != "" && db != "0" {
		if _, err := command(conn, rd, "SELECT", db); err != nil {
			return 0, err
		}
	}
	n, err := command(conn, rd, "LLEN", r.key)
	if err != nil {
		return 0, err
	}
	return float64(n), nil
}

// command sends a RESP array and parses a simple-string, error or integer reply.
func command(conn net.Conn, rd *bufio.Reader, args ...string) (int64, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := conn.Write([]byte(b.String())); err != nil {
		return 0, err
	}
	line, err := rd.ReadString('\n')
	if err != nil {
		return 0, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return 0, fmt.Errorf("redis %s: empty reply", args[0])
	}
	switch line[0] {
	case '+':
		return 0, nil
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '-':
		return 0, fmt.Errorf("redis %s: %s", args[0], line[1:])
	}
	return 0, fmt.Errorf("redis %s: unexpected reply %q", args[0], line)
}

// RabbitMQ reads the "messages" count (ready + unacked) from the
// management HTTP API.
type RabbitMQ struct {
	base        *url.URL
	vhost, name string
	client      *http.Client
}

func (q *RabbitMQ) Depth(ctx context.Context) (float64, error) {
	u := *q.base
	user := u.User
	u.User = nil
	// The default vhost is "/", which must reach the API as %2F.
	prefix := strings.TrimRight(u.Path, "/") + "/api/queues/"
	u.Path = prefix + q.vhost + "/" + q.name
	u.RawPath = prefix + url.PathEscape(q.vhost) + "/" + url.PathEscape(q.name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	if user != nil {
		pass, _ := user.Password()
		req.SetBasicAuth(user.Username(), pass)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("rabbitmq queue %s: %s", q.name, resp.Status)
	}
	var body struct {
		Messages float64 `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	return body.Messages, nil
}
