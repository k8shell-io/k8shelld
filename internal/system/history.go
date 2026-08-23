// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package system

import (
	"fmt"
	"sync"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/k8shelld"
)

const (
	// HistoryRetention is how long historical system/mount/docker usage
	// samples are kept in memory before aging out.
	HistoryRetention = 24 * time.Hour

	// MaxHistoryPoints caps the number of points returned for a single
	// series in one SystemInfoHistory response; a finer step than this
	// would allow is coarsened to respect it.
	MaxHistoryPoints = 720

	// DefaultHistoryRange is the window used when a request supplies
	// neither range nor a from/to pair.
	DefaultHistoryRange = 1 * time.Hour
)

// systemHistoryPoint is one recorded sample of system.go's CPU/memory
// fields. A nil field means that metric wasn't collected at this tick (the
// underlying refresh failed), so it shows as a gap rather than a stale or
// false value.
type systemHistoryPoint struct {
	time               time.Time
	cpuUsageMillicores *float64
	cpuLimitMillicores *float64
	memoryUsageMiB     *float64
	memLimitMiB        *float64
}

// mountHistoryPoint is one recorded sample of a single mount's usage.
type mountHistoryPoint struct {
	time       time.Time
	usedBytes  *uint64
	totalBytes *uint64
}

// dockerHistoryPoint is one recorded sample of Docker/Podman storage usage.
type dockerHistoryPoint struct {
	time         time.Time
	totalBytes   *uint64
	declaredSize *uint64
}

// history holds ring-buffer style, age-pruned samples backing the
// SystemInfoHistory RPC. Samples are appended once per Collect() tick from
// the same data already cached on SystemInfo, and pruned to HistoryRetention
// on every append.
type history struct {
	mu     sync.Mutex
	system []systemHistoryPoint
	mounts map[string][]mountHistoryPoint
	docker []dockerHistoryPoint
}

func newHistory() *history {
	return &history{mounts: make(map[string][]mountHistoryPoint)}
}

// record appends one tick's samples. sys is nil when that tick's CPU/memory
// refresh failed. mounts/docker are whatever is currently cached (possibly
// empty/nil on a failed or not-applicable collection) — records are only
// added for what's actually present, leaving the rest as a gap.
func (h *history) record(now time.Time, sys *systemHistoryPoint, mounts []k8shelld.MountUsage, docker *k8shelld.DockerUsage) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if sys != nil {
		h.system = append(h.system, *sys)
	}

	for _, m := range mounts {
		used, total := m.UsedBytes, m.TotalBytes
		h.mounts[m.MountPoint] = append(h.mounts[m.MountPoint], mountHistoryPoint{
			time: now, usedBytes: &used, totalBytes: &total,
		})
	}

	if docker != nil {
		total, declared := docker.TotalBytes, docker.DeclaredSize
		h.docker = append(h.docker, dockerHistoryPoint{time: now, totalBytes: &total, declaredSize: &declared})
	}

	h.pruneLocked(now)
}

// pruneLocked drops samples older than HistoryRetention. Samples are always
// appended in increasing time order, so the first non-expired sample marks
// where each series should be truncated from. Callers must hold h.mu.
func (h *history) pruneLocked(now time.Time) {
	cutoff := now.Add(-HistoryRetention)

	h.system = dropBefore(h.system, cutoff, func(p systemHistoryPoint) time.Time { return p.time })

	for mountPoint, pts := range h.mounts {
		pruned := dropBefore(pts, cutoff, func(p mountHistoryPoint) time.Time { return p.time })
		if len(pruned) == 0 {
			delete(h.mounts, mountPoint)
		} else {
			h.mounts[mountPoint] = pruned
		}
	}

	h.docker = dropBefore(h.docker, cutoff, func(p dockerHistoryPoint) time.Time { return p.time })
}

func dropBefore[T any](pts []T, cutoff time.Time, timeOf func(T) time.Time) []T {
	i := 0
	for i < len(pts) && timeOf(pts[i]).Before(cutoff) {
		i++
	}
	return pts[i:]
}

// resolveHistoryWindow resolves the [from, to) window for a query. When both
// from and to are set they take precedence over range; otherwise range (or
// DefaultHistoryRange when range is also empty) is applied back from now.
// The window is then clamped to [now-HistoryRetention, now].
func resolveHistoryWindow(reqFrom, reqTo, reqRange string, now time.Time) (from, to time.Time, err error) {
	if reqFrom != "" && reqTo != "" {
		from, err = time.Parse(time.RFC3339, reqFrom)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid from %q: %w", reqFrom, err)
		}
		to, err = time.Parse(time.RFC3339, reqTo)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid to %q: %w", reqTo, err)
		}
		if !to.After(from) {
			return time.Time{}, time.Time{}, fmt.Errorf("to must be after from")
		}
	} else {
		rangeDur := DefaultHistoryRange
		if reqRange != "" {
			rangeDur, err = time.ParseDuration(reqRange)
			if err != nil {
				return time.Time{}, time.Time{}, fmt.Errorf("invalid range %q: %w", reqRange, err)
			}
			if rangeDur <= 0 {
				return time.Time{}, time.Time{}, fmt.Errorf("range must be positive")
			}
		}
		to = now
		from = now.Add(-rangeDur)
	}

	if to.After(now) {
		to = now
	}
	if floor := now.Add(-HistoryRetention); from.Before(floor) {
		from = floor
	}
	if !to.After(from) {
		to = from.Add(time.Second)
	}

	return from, to, nil
}

// resolveHistoryStep resolves the sample interval for a query: it can't be
// finer than nativeInterval (the collection resolution), and is coarsened
// further if needed to keep the bucket count within MaxHistoryPoints.
func resolveHistoryStep(reqStep string, from, to time.Time, nativeInterval time.Duration) (time.Duration, error) {
	step := nativeInterval
	if reqStep != "" {
		d, err := time.ParseDuration(reqStep)
		if err != nil {
			return 0, fmt.Errorf("invalid step %q: %w", reqStep, err)
		}
		if d <= 0 {
			return 0, fmt.Errorf("step must be positive")
		}
		step = d
	}
	if step < nativeInterval {
		step = nativeInterval
	}

	if span := to.Sub(from); span > 0 {
		if minStep := span / MaxHistoryPoints; minStep > step {
			step = minStep
		}
	}

	return step, nil
}

func numHistoryBuckets(from, to time.Time, step time.Duration) int {
	span := to.Sub(from)
	if span <= 0 || step <= 0 {
		return 0
	}
	n := int(span / step)
	if span%step != 0 {
		n++
	}
	if n < 1 {
		n = 1
	}
	return n
}

func bucketIndex(t, from time.Time, step time.Duration) (int, bool) {
	if t.Before(from) {
		return 0, false
	}
	return int(t.Sub(from) / step), true
}

// bucketSystemPoints downsamples raw system samples into n evenly spaced
// buckets of width step starting at from, averaging present values per
// bucket and leaving a field nil where no sample carried it.
func bucketSystemPoints(points []systemHistoryPoint, from, to time.Time, step time.Duration) []k8shelld.SystemMetricsPoint {
	n := numHistoryBuckets(from, to, step)
	result := make([]k8shelld.SystemMetricsPoint, n)

	type acc struct {
		cpuUsage, cpuLimit, mem, memLimit     float64
		nCPUUsage, nCPULimit, nMem, nMemLimit int
	}
	sums := make([]acc, n)

	for _, p := range points {
		if !p.time.Before(to) {
			continue
		}
		idx, ok := bucketIndex(p.time, from, step)
		if !ok || idx >= n {
			continue
		}
		if p.cpuUsageMillicores != nil {
			sums[idx].cpuUsage += *p.cpuUsageMillicores
			sums[idx].nCPUUsage++
		}
		if p.cpuLimitMillicores != nil {
			sums[idx].cpuLimit += *p.cpuLimitMillicores
			sums[idx].nCPULimit++
		}
		if p.memoryUsageMiB != nil {
			sums[idx].mem += *p.memoryUsageMiB
			sums[idx].nMem++
		}
		if p.memLimitMiB != nil {
			sums[idx].memLimit += *p.memLimitMiB
			sums[idx].nMemLimit++
		}
	}

	for i := range result {
		result[i].Time = from.Add(time.Duration(i) * step).Format(time.RFC3339)
		if sums[i].nCPUUsage > 0 {
			v := sums[i].cpuUsage / float64(sums[i].nCPUUsage)
			result[i].CPUUsageMillicores = &v
		}
		if sums[i].nCPULimit > 0 {
			v := sums[i].cpuLimit / float64(sums[i].nCPULimit)
			result[i].CPULimitMillicores = &v
		}
		if sums[i].nMem > 0 {
			v := sums[i].mem / float64(sums[i].nMem)
			result[i].MemoryUsageMiB = &v
		}
		if sums[i].nMemLimit > 0 {
			v := sums[i].memLimit / float64(sums[i].nMemLimit)
			result[i].MemLimitMiB = &v
		}
	}
	return result
}

// bucketMountPoints downsamples one mount's raw samples the same way
// bucketSystemPoints does, averaging uint64 fields.
func bucketMountPoints(points []mountHistoryPoint, from, to time.Time, step time.Duration) []k8shelld.MountUsagePoint {
	n := numHistoryBuckets(from, to, step)
	result := make([]k8shelld.MountUsagePoint, n)

	type acc struct {
		usedSum, totalSum uint64
		nUsed, nTotal     int
	}
	sums := make([]acc, n)

	for _, p := range points {
		if !p.time.Before(to) {
			continue
		}
		idx, ok := bucketIndex(p.time, from, step)
		if !ok || idx >= n {
			continue
		}
		if p.usedBytes != nil {
			sums[idx].usedSum += *p.usedBytes
			sums[idx].nUsed++
		}
		if p.totalBytes != nil {
			sums[idx].totalSum += *p.totalBytes
			sums[idx].nTotal++
		}
	}

	for i := range result {
		result[i].Time = from.Add(time.Duration(i) * step).Format(time.RFC3339)
		if sums[i].nUsed > 0 {
			v := sums[i].usedSum / uint64(sums[i].nUsed)
			result[i].UsedBytes = &v
		}
		if sums[i].nTotal > 0 {
			v := sums[i].totalSum / uint64(sums[i].nTotal)
			result[i].TotalBytes = &v
		}
	}
	return result
}

// bucketDockerPoints downsamples raw Docker/Podman samples the same way
// bucketMountPoints does.
func bucketDockerPoints(points []dockerHistoryPoint, from, to time.Time, step time.Duration) []k8shelld.DockerUsagePoint {
	n := numHistoryBuckets(from, to, step)
	result := make([]k8shelld.DockerUsagePoint, n)

	type acc struct {
		totalSum, declaredSum uint64
		nTotal, nDeclared     int
	}
	sums := make([]acc, n)

	for _, p := range points {
		if !p.time.Before(to) {
			continue
		}
		idx, ok := bucketIndex(p.time, from, step)
		if !ok || idx >= n {
			continue
		}
		if p.totalBytes != nil {
			sums[idx].totalSum += *p.totalBytes
			sums[idx].nTotal++
		}
		if p.declaredSize != nil {
			sums[idx].declaredSum += *p.declaredSize
			sums[idx].nDeclared++
		}
	}

	for i := range result {
		result[i].Time = from.Add(time.Duration(i) * step).Format(time.RFC3339)
		if sums[i].nTotal > 0 {
			v := sums[i].totalSum / uint64(sums[i].nTotal)
			result[i].TotalBytes = &v
		}
		if sums[i].nDeclared > 0 {
			v := sums[i].declaredSum / uint64(sums[i].nDeclared)
			result[i].DeclaredSize = &v
		}
	}
	return result
}
