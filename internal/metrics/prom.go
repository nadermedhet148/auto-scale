// Package metrics scrapes request rate and latency from Traefik's
// Prometheus endpoint.
package metrics

import (
	"bufio"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Sample is one line of the Prometheus text exposition format.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

var errMalformed = errors.New("malformed sample")

// Parse reads the Prometheus text format, skipping comments and malformed lines.
func Parse(r io.Reader) ([]Sample, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var out []Sample
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		if s, err := parseLine(line); err == nil {
			out = append(out, s)
		}
	}
	return out, sc.Err()
}

func parseLine(line string) (Sample, error) {
	var s Sample
	i := strings.IndexAny(line, "{ ")
	if i <= 0 {
		return s, errMalformed
	}
	s.Name = line[:i]
	rest := line[i:]
	if rest[0] == '{' {
		end := closingBrace(rest)
		if end < 0 {
			return s, errMalformed
		}
		lbls, err := parseLabels(rest[1:end])
		if err != nil {
			return s, err
		}
		s.Labels = lbls
		rest = rest[end+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return s, errMalformed
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return s, err
	}
	s.Value = v
	return s, nil
}

func closingBrace(s string) int {
	inQuote := false
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if inQuote {
				i++
			}
		case '"':
			inQuote = !inQuote
		case '}':
			if !inQuote {
				return i
			}
		}
	}
	return -1
}

func parseLabels(s string) (map[string]string, error) {
	m := map[string]string{}
	for {
		s = strings.TrimLeft(s, " ,")
		if s == "" {
			return m, nil
		}
		eq := strings.IndexByte(s, '=')
		if eq < 0 || eq+1 >= len(s) || s[eq+1] != '"' {
			return nil, errMalformed
		}
		key := strings.TrimSpace(s[:eq])
		s = s[eq+2:]
		var b strings.Builder
		i := 0
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				if s[i] == 'n' {
					b.WriteByte('\n')
					continue
				}
			}
			b.WriteByte(s[i])
		}
		if i >= len(s) {
			return nil, errMalformed
		}
		m[key] = b.String()
		s = s[i+1:]
	}
}

// Quantile estimates quantile q from cumulative histogram buckets keyed by
// their upper bound, interpolating linearly like PromQL's histogram_quantile.
func Quantile(q float64, buckets map[float64]float64) (float64, bool) {
	les := make([]float64, 0, len(buckets))
	for le := range buckets {
		les = append(les, le)
	}
	sort.Float64s(les)
	if len(les) == 0 || !math.IsInf(les[len(les)-1], 1) {
		return 0, false
	}
	total := buckets[les[len(les)-1]]
	if total <= 0 {
		return 0, false
	}
	rank := q * total
	prevLe, prevCount := 0.0, 0.0
	for _, le := range les {
		count := buckets[le]
		if count >= rank {
			if math.IsInf(le, 1) {
				return prevLe, true
			}
			if count == prevCount {
				return le, true
			}
			// Clamp: float rounding can otherwise land just past the bucket bound.
			v := prevLe + (le-prevLe)*(rank-prevCount)/(count-prevCount)
			return math.Min(math.Max(v, prevLe), le), true
		}
		prevLe, prevCount = le, count
	}
	return prevLe, true
}
