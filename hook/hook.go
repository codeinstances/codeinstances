// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package hook lets a Go program's clock and random numbers be recorded and
// replayed by codeinstances.
//
// The proxy sees a program's HTTP traffic from outside. The clock and random
// numbers it can't see, so a program that wants exact replay asks for them
// through this package instead of time.Now and math/rand:
//
//	start := hook.Now()
//	id := hook.Uint64()
//	client := hook.Client() // trusts the recording's certificate authority
//
// Run normally, the package is a thin layer over the real clock and
// crypto/rand. Run under codeinstances record, each value is also written to
// the instance. Run under codeinstances replay, the recorded values come
// back in the same order; if the program asks for more than were recorded,
// it gets real ones and the replay is reported as diverged.
package hook

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Environment variables codeinstances sets for the program it runs.
const (
	EnvMode      = "CODEINSTANCES_MODE"       // "record" or "replay"
	EnvValuesIn  = "CODEINSTANCES_VALUES_IN"  // replay: the recorded values
	EnvValuesOut = "CODEINSTANCES_VALUES_OUT" // every value handed out
	EnvCA        = "CODEINSTANCES_CA"         // the run's CA certificate, PEM
)

// Line is one value in the values files, one JSON object per line.
type Line struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
	// Live marks a value replay had to make up because the recording ran out.
	Live bool `json:"live,omitempty"`
}

var (
	once   sync.Once
	mu     sync.Mutex
	mode   string
	out    *os.File
	queues map[string][]string
)

func setup() {
	mode = os.Getenv(EnvMode)
	if mode != "record" && mode != "replay" {
		mode = ""
		return
	}
	if path := os.Getenv(EnvValuesOut); path != "" {
		out, _ = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	}
	queues = map[string][]string{}
	if mode == "replay" {
		if f, err := os.Open(os.Getenv(EnvValuesIn)); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64<<10), 16<<20)
			for sc.Scan() {
				var l Line
				if json.Unmarshal(sc.Bytes(), &l) == nil {
					queues[l.Kind] = append(queues[l.Kind], l.Value)
				}
			}
			f.Close()
		}
	}
}

// Active reports whether the program runs under codeinstances.
func Active() bool {
	once.Do(setup)
	return mode != ""
}

// value hands out the next value of a kind: the recorded one when replaying,
// otherwise the one live makes; and notes it.
func value(kind string, live func() string) string {
	once.Do(setup)
	if mode == "" {
		return live()
	}
	mu.Lock()
	defer mu.Unlock()
	l := Line{Kind: kind}
	if q := queues[kind]; mode == "replay" && len(q) > 0 {
		l.Value = q[0]
		queues[kind] = q[1:]
	} else {
		l.Value = live()
		l.Live = mode == "replay"
	}
	if out != nil {
		b, _ := json.Marshal(l)
		out.Write(append(b, '\n'))
	}
	return l.Value
}

// Now returns the current time, recorded or replayed. It carries no
// monotonic reading, so the same instant prints the same way in a recording
// and in its replay.
func Now() time.Time {
	s := value("clock", func() string { return time.Now().Round(0).Format(time.RFC3339Nano) })
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Now().Round(0)
	}
	return t.In(time.Local)
}

// Since is time since t by Now.
func Since(t time.Time) time.Duration { return Now().Sub(t) }

// Uint64 returns a random number, recorded or replayed.
func Uint64() uint64 {
	s := value("random", func() string {
		var b [8]byte
		rand.Read(b[:])
		return strconv.FormatUint(binary.LittleEndian.Uint64(b[:]), 10)
	})
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

// Int63 returns a non-negative random int64.
func Int63() int64 { return int64(Uint64() >> 1) }

// Intn returns a random int in [0, n). It panics if n <= 0.
func Intn(n int) int {
	if n <= 0 {
		panic("hook.Intn: n <= 0")
	}
	return int(Uint64() % uint64(n))
}

// Float64 returns a random float64 in [0.0, 1.0).
func Float64() float64 { return float64(Uint64()>>11) / (1 << 53) }

// Read fills p with random bytes, recorded or replayed as one value.
func Read(p []byte) (int, error) {
	s := value("bytes", func() string {
		b := make([]byte, len(p))
		rand.Read(b)
		return base64.StdEncoding.EncodeToString(b)
	})
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, err
	}
	return copy(p, b), nil
}

// Transport is a copy of http.DefaultTransport that also trusts the run's
// certificate authority, so HTTPS requests reach the proxy on any system.
// (On Linux, most programs trust it already through SSL_CERT_FILE; macOS and
// Windows Go programs need this.)
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if pem, err := os.ReadFile(os.Getenv(EnvCA)); err == nil && os.Getenv(EnvCA) != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(pem)
		t.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return t
}

// Client is an http.Client using Transport.
func Client() *http.Client { return &http.Client{Transport: Transport()} }
