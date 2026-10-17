// Package replay records live load to CSV and replays recordings through the
// decision engine offline, so policies can be tuned without touching Docker.
//
// Recordings hold service-wide totals (CPU as summed % across replicas,
// rps across replicas, total queue backlog), not per-replica values. That
// lets the simulator re-divide the same load across a different replica count.
// Latency is recorded as observed and passed through unchanged.
package replay

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"dockscale/internal/config"
)

// Row is one tick of one service.
type Row struct {
	At       time.Time
	Service  string
	Replicas int
	Totals   map[string]float64
}

func header() []string {
	return append([]string{"timestamp", "service", "replicas"}, config.AllMetrics...)
}

// Recorder appends rows to a CSV file. It is safe for concurrent use.
type Recorder struct {
	mu sync.Mutex
	f  *os.File
	w  *csv.Writer
}

func NewRecorder(path string) (*Recorder, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	r := &Recorder{f: f, w: csv.NewWriter(f)}
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		_ = r.w.Write(header())
		r.w.Flush()
	}
	return r, nil
}

func (r *Recorder) Record(row Row) error {
	return r.write(rowRecord(row))
}

func (r *Recorder) write(rec []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.w.Write(rec); err != nil {
		return err
	}
	r.w.Flush()
	return r.w.Error()
}

func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.w.Flush()
	return r.f.Close()
}

func rowRecord(row Row) []string {
	rec := []string{row.At.UTC().Format(time.RFC3339), row.Service, strconv.Itoa(row.Replicas)}
	for _, m := range config.AllMetrics {
		if v, ok := row.Totals[m]; ok {
			rec = append(rec, strconv.FormatFloat(v, 'f', 3, 64))
		} else {
			rec = append(rec, "")
		}
	}
	return rec
}

// WriteRows writes a complete recording, header included.
func WriteRows(w io.Writer, rows []Row) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(header()); err != nil {
		return err
	}
	for _, row := range rows {
		if err := cw.Write(rowRecord(row)); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// ReadRows parses a recording, keeping rows for one service ("" keeps all).
// Columns are matched by header name; empty cells mean "not observed".
func ReadRows(r io.Reader, service string) ([]Row, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	head, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[h] = i
	}
	if _, ok := col["timestamp"]; !ok {
		return nil, fmt.Errorf("recording has no timestamp column")
	}
	cell := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return rec[i]
		}
		return ""
	}

	var rows []Row
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if service != "" && cell(rec, "service") != "" && cell(rec, "service") != service {
			continue
		}
		at, err := time.Parse(time.RFC3339, cell(rec, "timestamp"))
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		row := Row{At: at, Service: cell(rec, "service"), Totals: map[string]float64{}}
		if s := cell(rec, "replicas"); s != "" {
			row.Replicas, _ = strconv.Atoi(s)
		}
		for _, m := range config.AllMetrics {
			s := cell(rec, m)
			if s == "" {
				continue
			}
			v, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, fmt.Errorf("line %d, %s: %w", line, m, err)
			}
			row.Totals[m] = v
		}
		rows = append(rows, row)
	}
	return rows, nil
}
