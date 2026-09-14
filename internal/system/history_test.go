// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package system

import (
	"testing"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/k8shelld"
	commonmodels "github.com/k8shell-io/common/pkg/models"
)

func TestResolveHistoryWindow(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	t.Run("defaults to DefaultHistoryRange when nothing is set", func(t *testing.T) {
		from, to, err := resolveHistoryWindow("", "", "", now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !to.Equal(now) {
			t.Errorf("to = %v, want %v", to, now)
		}
		if want := now.Add(-DefaultHistoryRange); !from.Equal(want) {
			t.Errorf("from = %v, want %v", from, want)
		}
	})

	t.Run("range shorthand applies back from now", func(t *testing.T) {
		from, to, err := resolveHistoryWindow("", "", "30m", now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !to.Equal(now) {
			t.Errorf("to = %v, want %v", to, now)
		}
		if want := now.Add(-30 * time.Minute); !from.Equal(want) {
			t.Errorf("from = %v, want %v", from, want)
		}
	})

	t.Run("from/to take precedence over range", func(t *testing.T) {
		reqFrom := now.Add(-2 * time.Hour).Format(time.RFC3339)
		reqTo := now.Add(-1 * time.Hour).Format(time.RFC3339)
		from, to, err := resolveHistoryWindow(reqFrom, reqTo, "5m", now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := now.Add(-2 * time.Hour); !from.Equal(want) {
			t.Errorf("from = %v, want %v", from, want)
		}
		if want := now.Add(-1 * time.Hour); !to.Equal(want) {
			t.Errorf("to = %v, want %v", to, want)
		}
	})

	t.Run("to is clamped to now", func(t *testing.T) {
		reqFrom := now.Add(-1 * time.Hour).Format(time.RFC3339)
		reqTo := now.Add(1 * time.Hour).Format(time.RFC3339)
		_, to, err := resolveHistoryWindow(reqFrom, reqTo, "", now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !to.Equal(now) {
			t.Errorf("to = %v, want clamped to now (%v)", to, now)
		}
	})

	t.Run("from is clamped to the retention floor", func(t *testing.T) {
		from, _, err := resolveHistoryWindow("", "", "1000h", now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := now.Add(-HistoryRetention); !from.Equal(want) {
			t.Errorf("from = %v, want retention floor %v", from, want)
		}
	})

	t.Run("rejects to before from", func(t *testing.T) {
		reqFrom := now.Format(time.RFC3339)
		reqTo := now.Add(-1 * time.Hour).Format(time.RFC3339)
		if _, _, err := resolveHistoryWindow(reqFrom, reqTo, "", now); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("rejects malformed range", func(t *testing.T) {
		if _, _, err := resolveHistoryWindow("", "", "not-a-duration", now); err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestResolveHistoryStep(t *testing.T) {
	now := time.Now()
	native := 30 * time.Second

	t.Run("defaults to native interval", func(t *testing.T) {
		step, err := resolveHistoryStep("", now.Add(-time.Hour), now, native)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if step != native {
			t.Errorf("step = %v, want %v", step, native)
		}
	})

	t.Run("clamps a finer request up to native interval", func(t *testing.T) {
		step, err := resolveHistoryStep("1s", now.Add(-time.Hour), now, native)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if step != native {
			t.Errorf("step = %v, want native %v", step, native)
		}
	})

	t.Run("coarsens to respect MaxHistoryPoints", func(t *testing.T) {
		from := now.Add(-HistoryRetention)
		step, err := resolveHistoryStep("", from, now, native)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		n := numHistoryBuckets(from, now, step)
		if n > MaxHistoryPoints {
			t.Errorf("buckets = %d, want <= %d (step=%v)", n, MaxHistoryPoints, step)
		}
	})

	t.Run("rejects malformed step", func(t *testing.T) {
		if _, err := resolveHistoryStep("nope", now.Add(-time.Hour), now, native); err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("rejects non-positive step", func(t *testing.T) {
		if _, err := resolveHistoryStep("0s", now.Add(-time.Hour), now, native); err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestHistoryRecordAndPrune(t *testing.T) {
	h := newHistory()
	base := time.Now().Add(-2 * HistoryRetention)

	// Old sample that should be pruned away as newer samples come in.
	h.record(base, &systemHistoryPoint{time: base, cpuUsageMillicores: floatPtr(1)}, nil, nil)

	now := time.Now()
	h.record(now, &systemHistoryPoint{time: now, cpuUsageMillicores: floatPtr(2)}, nil, nil)

	if len(h.system) != 1 {
		t.Fatalf("len(h.system) = %d, want 1 (old sample should have aged out)", len(h.system))
	}
	if *h.system[0].cpuUsageMillicores != 2 {
		t.Errorf("remaining sample = %v, want 2", *h.system[0].cpuUsageMillicores)
	}
}

func TestHistoryRecordMountsAndDocker(t *testing.T) {
	h := newHistory()
	now := time.Now()

	mounts := []k8shelld.MountUsage{
		{MountPoint: "/data", UsedBytes: 100, TotalBytes: 1000},
	}
	docker := &k8shelld.DockerUsage{TotalBytes: 500, DeclaredSize: 2000}

	h.record(now, nil, mounts, docker)

	if _, ok := h.mounts["/data"]; !ok {
		t.Fatal("expected /data mount history to be recorded")
	}
	if len(h.docker) != 1 {
		t.Fatalf("len(h.docker) = %d, want 1", len(h.docker))
	}
	if len(h.system) != 0 {
		t.Errorf("len(h.system) = %d, want 0 (nil sys point should not be recorded)", len(h.system))
	}
}

func TestBucketSystemPointsAveragesAndGaps(t *testing.T) {
	from := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	step := time.Minute
	to := from.Add(3 * step)

	points := []systemHistoryPoint{
		{time: from, cpuUsageMillicores: floatPtr(10)},
		{time: from.Add(30 * time.Second), cpuUsageMillicores: floatPtr(20)},
		// bucket 1 (from+1min..from+2min) has no samples: should be a gap.
		{time: from.Add(2 * step), memoryUsageMiB: floatPtr(50)},
	}

	result := bucketSystemPoints(points, from, to, step)
	if len(result) != 3 {
		t.Fatalf("len(result) = %d, want 3", len(result))
	}

	if result[0].CPUUsageMillicores == nil {
		t.Fatal("bucket 0 CPUUsageMillicores should be set")
	} else if want := 15.0; *result[0].CPUUsageMillicores != want {
		t.Errorf("bucket 0 CPUUsageMillicores = %v, want %v (average of 10 and 20)", *result[0].CPUUsageMillicores, want)
	}

	if result[1].CPUUsageMillicores != nil {
		t.Errorf("bucket 1 CPUUsageMillicores = %v, want nil (gap)", *result[1].CPUUsageMillicores)
	}

	if result[2].MemoryUsageMiB == nil || *result[2].MemoryUsageMiB != 50 {
		t.Errorf("bucket 2 MemoryUsageMiB = %v, want 50", result[2].MemoryUsageMiB)
	}
}

func TestGetSystemInfoHistoryEndToEnd(t *testing.T) {
	blueprint := &commonmodels.Blueprint{}
	blueprint.Podman.Enabled = true

	s := NewSystemInfo(nil, blueprint)
	s.mu.Lock()
	s.collectionInterval = 30 * time.Second
	s.mu.Unlock()

	now := time.Now()
	for i := 0; i < 3; i++ {
		t := now.Add(time.Duration(i) * 30 * time.Second)
		s.history.record(t,
			&systemHistoryPoint{time: t, cpuUsageMillicores: floatPtr(float64(i))},
			[]k8shelld.MountUsage{{MountPoint: "/data", UsedBytes: uint64(i), TotalBytes: 1000}},
			&k8shelld.DockerUsage{TotalBytes: uint64(i * 10)},
		)
	}

	hist, err := s.GetSystemInfoHistory(k8shelld.SystemInfoHistoryQuery{Range: "5m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hist.Docker == nil {
		t.Fatal("expected Docker to be non-nil since Podman is enabled")
	}
	if _, ok := hist.Mounts["/data"]; !ok {
		t.Fatal("expected /data in Mounts")
	}
	if len(hist.System) == 0 {
		t.Fatal("expected non-empty System series")
	}
}

func TestGetSystemInfoHistoryDockerUnavailable(t *testing.T) {
	s := NewSystemInfo(nil, &commonmodels.Blueprint{})
	s.mu.Lock()
	s.collectionInterval = 30 * time.Second
	s.mu.Unlock()

	hist, err := s.GetSystemInfoHistory(k8shelld.SystemInfoHistoryQuery{Range: "5m"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hist.Docker != nil {
		t.Error("expected Docker to be nil when Podman is not enabled")
	}
}

func TestGetSystemInfoHistoryInvalidQuery(t *testing.T) {
	s := NewSystemInfo(nil, &commonmodels.Blueprint{})
	if _, err := s.GetSystemInfoHistory(k8shelld.SystemInfoHistoryQuery{Range: "not-a-duration"}); err == nil {
		t.Fatal("expected error for malformed range")
	}
}
