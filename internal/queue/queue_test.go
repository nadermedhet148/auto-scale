package queue

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dockscale/internal/config"
)

// fakeRedis answers AUTH/SELECT with +OK and LLEN with :42, recording commands.
func fakeRedis(t *testing.T) (addr string, got chan []string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan []string, 8)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		rd := bufio.NewReader(conn)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			var n int
			if _, err := fmt.Sscanf(line, "*%d", &n); err != nil {
				return
			}
			args := make([]string, n)
			for i := range args {
				rd.ReadString('\n') // $len
				a, _ := rd.ReadString('\n')
				args[i] = strings.TrimRight(a, "\r\n")
			}
			got <- args
			if args[0] == "LLEN" {
				conn.Write([]byte(":42\r\n"))
			} else {
				conn.Write([]byte("+OK\r\n"))
			}
		}
	}()
	return ln.Addr().String(), got
}

func TestRedisDepth(t *testing.T) {
	addr, got := fakeRedis(t)
	src, err := New(config.Queue{Kind: config.QueueRedisList, URL: "redis://:secret@" + addr + "/2", Name: "jobs"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := src.Depth(context.Background())
	if err != nil || n != 42 {
		t.Fatalf("depth = %v, %v", n, err)
	}
	want := []string{"AUTH secret", "SELECT 2", "LLEN jobs"}
	for _, w := range want {
		if g := strings.Join(<-got, " "); g != w {
			t.Fatalf("command %q, want %q", g, w)
		}
	}
}

func TestRabbitMQDepth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, _ := r.BasicAuth(); u != "guest" || p != "guest" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.EscapedPath() != "/api/queues/%2F/jobs" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"name":"jobs","messages":17,"messages_ready":12}`))
	}))
	defer srv.Close()

	src, err := New(config.Queue{Kind: config.QueueRabbitMQ, URL: strings.Replace(srv.URL, "http://", "http://guest:guest@", 1), Name: "jobs", VHost: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := src.Depth(context.Background()); err != nil || n != 17 {
		t.Fatalf("depth = %v, %v", n, err)
	}
}
