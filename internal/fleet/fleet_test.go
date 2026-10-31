package fleet

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tendant/qifa/internal/config"
)

func servers() map[string]config.Server {
	return map[string]config.Server{
		"worker": {Hosts: []string{"h2", "h3"}},
		"web":    {Hosts: []string{"h1", "h2"}},
		"cron":   {Hosts: []string{"h3"}},
	}
}

func TestOrderAndHosts(t *testing.T) {
	if got := strings.Join(OrderedRoles(servers()), ","); got != "web,cron,worker" {
		t.Fatalf("roles = %s", got)
	}
	if got := strings.Join(Hosts(servers()), ","); got != "h1,h2,h3" {
		t.Fatalf("hosts = %s", got)
	}
	if got := strings.Join(RolesOn(servers(), "h3"), ","); got != "cron,worker" {
		t.Fatalf("roles on h3 = %s", got)
	}
}

// A failed batch stops the rollout; hosts of a batch run together.
func TestBatchesStopAtFirstFailedBatch(t *testing.T) {
	two := 2
	var ran atomic.Int32
	var skipped string
	err := Batches(context.Background(), []string{"a", "b", "c", "d"}, config.Rollout{BatchSize: &two},
		func(f string, a ...any) { skipped = f },
		func(_ context.Context, host string) error {
			ran.Add(1)
			if host == "b" {
				return errors.New("b broke")
			}
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), "b broke") {
		t.Fatalf("err = %v", err)
	}
	if ran.Load() != 2 || skipped == "" {
		t.Fatalf("ran %d hosts, skip log %q; want the first batch only", ran.Load(), skipped)
	}
	zero := 0
	ran.Store(0)
	if err := Batches(context.Background(), []string{"a", "b", "c"}, config.Rollout{BatchSize: &zero}, nil,
		func(context.Context, string) error { ran.Add(1); return nil }); err != nil || ran.Load() != 3 {
		t.Fatalf("batch_size 0 = all hosts at once: %v, ran %d", err, ran.Load())
	}
}

func TestPrefixWriterKeepsLinesWhole(t *testing.T) {
	var out bytes.Buffer
	var mu sync.Mutex
	a := &PrefixWriter{Prefix: "h1 | ", Out: &out, Mu: &mu}
	b := &PrefixWriter{Prefix: "h2 | ", Out: &out, Mu: &mu}
	a.Write([]byte("one\ntw"))
	b.Write([]byte("x\n"))
	a.Write([]byte("o\nthree"))
	a.Flush()
	if want := "h1 | one\nh2 | x\nh1 | two\nh1 | three\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

func TestDiffEnvNamesKeysOnly(t *testing.T) {
	joined := strings.Join(DiffEnv(map[string]string{"A": "secret1", "B": "x"}, map[string]string{"A": "secret2", "C": "y"}), "\n")
	for _, want := range []string{"~ A", "+ B", "- C"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
	if strings.Contains(joined, "secret") {
		t.Fatalf("diff printed a value: %s", joined)
	}
}
