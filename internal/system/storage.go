package system

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/k8shelld"
)

func GetMountUsages() ([]k8shelld.MountUsage, error) {
	entries, err := readMountInfo("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(entries))
	out := make([]k8shelld.MountUsage, 0, len(entries))

	for _, e := range entries {
		mp := e.mountPoint
		if mp == "" || !strings.HasPrefix(mp, "/") {
			continue
		}
		if _, ok := seen[mp]; ok {
			continue
		}
		seen[mp] = struct{}{}

		var st syscall.Statfs_t
		if err := syscall.Statfs(mp, &st); err != nil {
			// Don’t fail the whole request—some pseudo mounts may deny statfs.
			continue
		}

		bsize, ok := u64FromNonNegI64(st.Bsize)
		if !ok || bsize == 0 {
			continue
		}

		total := uint64(st.Blocks) * bsize
		free := uint64(st.Bfree) * bsize
		avail := uint64(st.Bavail) * bsize
		used := uint64(0)
		if total >= free {
			used = total - free
		}

		opts := splitCSV(e.mountOptions)
		ro := hasOpt(opts, "ro")

		fs := e.fsType
		src := e.source

		out = append(out, k8shelld.MountUsage{
			MountPoint:     mp,
			Source:         src,
			FSType:         fs,
			Options:        opts,
			ReadOnly:       ro,
			IsLikelyTemp:   fs == "overlay" || fs == "tmpfs",
			TotalBytes:     total,
			UsedBytes:      used,
			FreeBytes:      free,
			AvailableBytes: avail,
			TotalInodes:    uint64(st.Files),
			FreeInodes:     uint64(st.Ffree),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].MountPoint < out[j].MountPoint })
	return out, nil
}

type mountInfoEntry struct {
	mountPoint   string
	mountOptions string
	fsType       string
	source       string
}

func readMountInfo(path string) ([]mountInfoEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open mountinfo: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// mountinfo lines can be long
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var out []mountInfoEntry
	for sc.Scan() {
		line := sc.Text()
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}

		pre := strings.Fields(parts[0])
		post := strings.Fields(parts[1])

		// pre: mountID parentID major:minor root mountPoint mountOptions ...
		// post: fsType source superOptions
		if len(pre) < 6 || len(post) < 2 {
			continue
		}

		mp := unescapeMountInfoPath(pre[4])
		out = append(out, mountInfoEntry{
			mountPoint:   mp,
			mountOptions: pre[5],
			fsType:       post[0],
			source:       post[1],
		})
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan mountinfo: %w", err)
	}
	return out, nil
}

func unescapeMountInfoPath(s string) string {
	// mountinfo escapes space/tab/newline/backslash as octal: \040 \011 \012 \134
	if !strings.Contains(s, `\`) {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+3 < len(s) {
			if isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
				v, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
				if err == nil {
					b = append(b, byte(v))
					i += 4
					continue
				}
			}
		}
		b = append(b, s[i])
		i++
	}
	return string(b)
}

// PodmanDetails carries Podman-specific metadata that has no equivalent in
// the Docker-compat DockerUsage proto. It is populated from /libpod/info.
type PodmanDetails struct {
	PodmanVersion     string `json:"podmanVersion"` // e.g. "5.8.1"
	GraphDriver       string `json:"graphDriver"`   // e.g. "overlay"
	RunRoot           string `json:"runRoot"`       // e.g. "/tmp/storage-run-1000/containers"
	ContainersTotal   int    `json:"containersTotal"`
	ContainersRunning int    `json:"containersRunning"`
	ContainersPaused  int    `json:"containersPaused"`
	ContainersStopped int    `json:"containersStopped"`
}

func GetDockerUsage(ctx context.Context) (*k8shelld.DockerUsage, *PodmanDetails, error) {
	// Avoid importing internal/config here (it imports system -> would cycle).
	candidates := []string{
		"/run/podman/podman.sock",
		"/var/run/podman/podman.sock",
		"/var/run/docker.sock",
		"/var/run/docker/docker.sock",
	}

	var sock string
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && (st.Mode()&os.ModeSocket) != 0 {
			sock = p
			break
		}
	}
	if sock == "" {
		return nil, nil, errors.New("docker socket not found")
	}

	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", sock, 2*time.Second)
			},
		},
	}

	type dockerVersion struct {
		APIVersion string `json:"ApiVersion"`
		Version    string `json:"Version"` // Podman's own semver, e.g. "5.8.1"
	}

	apiPrefix := ""    // Docker compat prefix, e.g. /v1.44
	podmanPrefix := "" // Podman native prefix, e.g. /v5.8.1
	{
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/version", nil)
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			var v dockerVersion
			_ = json.NewDecoder(resp.Body).Decode(&v)
			if v.APIVersion != "" {
				apiPrefix = "/v" + v.APIVersion
			}
			if v.Version != "" {
				podmanPrefix = "/v" + v.Version
			}
		}
	}

	get := func(path string, out any) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("docker %s: status %s", path, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}

	du := &k8shelld.DockerUsage{SocketPath: sock}
	pd := &PodmanDetails{}
	if podmanPrefix != "" {
		pd.PodmanVersion = strings.TrimPrefix(podmanPrefix, "/v")
	}

	// Populate from the Podman-native /libpod/info endpoint.
	// Podman computes graphRootUsed/Allocated via Statfs inside its own container,
	// so they reflect the true on-disk footprint even when Podman runs as a sidecar.
	var podmanStoreUsed, podmanStoreAllocated uint64
	{
		type podmanContainerStore struct {
			Number  int `json:"number"`
			Running int `json:"running"`
			Paused  int `json:"paused"`
			Stopped int `json:"stopped"`
		}
		type podmanStore struct {
			GraphRoot          string               `json:"graphRoot"`
			GraphRootAllocated int64                `json:"graphRootAllocated"`
			GraphRootUsed      int64                `json:"graphRootUsed"`
			GraphDriverName    string               `json:"graphDriverName"`
			RunRoot            string               `json:"runRoot"`
			ContainerStore     podmanContainerStore `json:"containerStore"`
		}
		type podmanInfo struct {
			Store podmanStore `json:"store"`
		}
		var pi podmanInfo
		libpodPath := "/libpod/info"
		if podmanPrefix != "" {
			libpodPath = podmanPrefix + "/libpod/info"
		}
		if err := get(libpodPath, &pi); err != nil && podmanPrefix != "" {
			// fallback to unversioned
			err = get("/libpod/info", &pi)
			_ = err
		}
		if pi.Store.GraphRoot != "" || pi.Store.GraphRootUsed != 0 {
			if pi.Store.GraphRoot != "" {
				du.DockerRootDir = pi.Store.GraphRoot
			}
			if u, ok := u64FromNonNegI64(pi.Store.GraphRootUsed); ok {
				podmanStoreUsed = u
			}
			if u, ok := u64FromNonNegI64(pi.Store.GraphRootAllocated); ok {
				podmanStoreAllocated = u
			}
			pd.GraphDriver = pi.Store.GraphDriverName
			pd.RunRoot = pi.Store.RunRoot
			pd.ContainersTotal = pi.Store.ContainerStore.Number
			pd.ContainersRunning = pi.Store.ContainerStore.Running
			pd.ContainersPaused = pi.Store.ContainerStore.Paused
			pd.ContainersStopped = pi.Store.ContainerStore.Stopped
		}
	}

	// Fall back to the Docker-compat /info for DockerRootDir if libpod didn't provide it.
	if du.DockerRootDir == "" {
		type dockerInfo struct {
			DockerRootDir string `json:"DockerRootDir"`
		}
		var inf dockerInfo
		if err := get(apiPrefix+"/info", &inf); err == nil {
			du.DockerRootDir = inf.DockerRootDir
		} else if apiPrefix != "" {
			if err2 := get("/info", &inf); err2 == nil {
				du.DockerRootDir = inf.DockerRootDir
			}
		}
	}

	_ = podmanStoreAllocated // available if blueprint doesn't override DeclaredSize

	// /system/df response shape (partial)
	type dfImage struct {
		// Docker API may return either/both depending on version/flags.
		// Size is the on-disk size; VirtualSize exists on some versions.
		Size        int64 `json:"Size"`
		VirtualSize int64 `json:"VirtualSize"`
	}
	type dfContainer struct {
		// SizeRootFs usually requires ?size=1; some daemons return SizeRw instead.
		SizeRootFs int64 `json:"SizeRootFs"`
		SizeRw     int64 `json:"SizeRw"`
	}
	type dfVolume struct {
		UsageData *struct {
			Size int64 `json:"Size"`
		} `json:"UsageData"`
	}
	type dfBuildCache struct {
		Size int64 `json:"Size"`
	}
	type dfResp struct {
		Images     []dfImage      `json:"Images"`
		Containers []dfContainer  `json:"Containers"`
		Volumes    []dfVolume     `json:"Volumes"`
		BuildCache []dfBuildCache `json:"BuildCache"`
	}

	var df dfResp
	if err := get(apiPrefix+"/system/df", &df); err != nil && apiPrefix != "" {
		_ = get("/system/df", &df)
	}

	var images, containersRw, containersRootFs, volumes, cache uint64

	for _, i := range df.Images {
		if u, ok := u64FromNonNegI64(i.Size); ok && u > 0 {
			images += u
			continue
		}
		if u, ok := u64FromNonNegI64(i.VirtualSize); ok && u > 0 {
			images += u
		}
	}

	for _, c := range df.Containers {
		if u, ok := u64FromNonNegI64(c.SizeRw); ok && u > 0 {
			containersRw += u
		}
		if u, ok := u64FromNonNegI64(c.SizeRootFs); ok && u > 0 {
			containersRootFs += u
		}
	}

	for _, v := range df.Volumes {
		if v.UsageData != nil {
			if u, ok := u64FromNonNegI64(v.UsageData.Size); ok && u > 0 {
				volumes += u
			}
		}
	}

	for _, bc := range df.BuildCache {
		if u, ok := u64FromNonNegI64(bc.Size); ok && u > 0 {
			cache += u
		}
	}

	du.ImagesBytes = images
	du.ContainersBytes = containersRw
	du.ContainersRootFsBytes = containersRootFs
	du.VolumesBytes = volumes
	du.BuildCacheBytes = cache

	// Prefer the actual on-disk usage reported by Podman's /libpod/info
	// (store.graphRootUsed). Podman computes this via Statfs inside its own
	// container, so it captures overlay driver overhead, internal databases,
	// and orphaned layers that /system/df misses. This also works correctly
	// when Podman runs as a sidecar (its storage is inaccessible to k8shelld
	// directly). Fall back to the sum of API-reported logical sizes otherwise.
	if podmanStoreUsed > 0 {
		du.TotalBytes = podmanStoreUsed
	} else {
		du.TotalBytes = images + containersRw + volumes + cache
	}

	// Best-effort API version visibility
	if apiPrefix != "" {
		du.APIVersion = strings.TrimPrefix(apiPrefix, "/v")
	}

	return du, pd, nil
}

// ** helpers

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func hasOpt(opts []string, needle string) bool {
	for _, o := range opts {
		if o == needle {
			return true
		}
	}
	return false
}

// parseSizeBytes parses Kubernetes-style quantities (compatible with resource.Quantity), e.g.:
// - Binary SI: Ki, Mi, Gi, Ti, Pi, Ei (base 1024)   e.g. "10Gi", "1.5Gi"
// - Decimal SI: n, u, m, k, M, G, T, P, E (base 10) e.g. "500M", "1G", "250m"
// - Scientific notation is accepted                  e.g. "1e3", "1.2e6"
func ParseSizeBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	var re = regexp.MustCompile(`^([+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)([a-zA-Z]{0,2})$`)
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid quantity %q", s)
	}

	numStr := m[1]
	suf := m[2]

	if strings.HasSuffix(suf, "B") || strings.HasSuffix(suf, "b") {
		return 0, fmt.Errorf("invalid quantity %q: suffix %q not supported (use Gi, G, Mi, etc.)", s, suf)
	}

	r, err := parseNumberToRat(numStr)
	if err != nil {
		return 0, fmt.Errorf("invalid quantity %q: %w", s, err)
	}
	if r.Sign() < 0 {
		return 0, fmt.Errorf("invalid quantity %q: must be non-negative", s)
	}

	if err := applyK8sSuffix(r, suf); err != nil {
		return 0, fmt.Errorf("invalid quantity %q: %w", s, err)
	}

	return ratCeilToUint64(r)
}

func parseNumberToRat(numStr string) (*big.Rat, error) {
	mant := numStr
	exp10 := 0
	if i := strings.IndexAny(numStr, "eE"); i >= 0 {
		mant = numStr[:i]
		e := numStr[i+1:]
		if e == "" {
			return nil, fmt.Errorf("bad exponent")
		}
		ee, err := strconv.Atoi(e)
		if err != nil {
			return nil, fmt.Errorf("bad exponent: %w", err)
		}
		exp10 = ee
	}

	sign := 1
	if strings.HasPrefix(mant, "+") {
		mant = strings.TrimPrefix(mant, "+")
	} else if strings.HasPrefix(mant, "-") {
		sign = -1
		mant = strings.TrimPrefix(mant, "-")
	}
	if mant == "" {
		return nil, fmt.Errorf("missing mantissa")
	}

	decimals := 0
	if dot := strings.IndexByte(mant, '.'); dot >= 0 {
		decimals = len(mant) - dot - 1
		mant = mant[:dot] + mant[dot+1:]
	}
	mant = strings.TrimSpace(mant)
	if mant == "" {
		return nil, fmt.Errorf("missing digits")
	}
	for i := 0; i < len(mant); i++ {
		if mant[i] < '0' || mant[i] > '9' {
			return nil, fmt.Errorf("invalid digit in mantissa")
		}
	}

	mantInt := new(big.Int)
	if _, ok := mantInt.SetString(mant, 10); !ok {
		return nil, fmt.Errorf("cannot parse mantissa")
	}
	if sign < 0 {
		mantInt.Neg(mantInt)
	}

	r := new(big.Rat).SetInt(mantInt)

	if decimals > 0 {
		r.Quo(r, new(big.Rat).SetInt(pow10Int(decimals)))
	}

	if exp10 > 0 {
		r.Mul(r, new(big.Rat).SetInt(pow10Int(exp10)))
	} else if exp10 < 0 {
		r.Quo(r, new(big.Rat).SetInt(pow10Int(-exp10)))
	}

	return r, nil
}

func applyK8sSuffix(r *big.Rat, suf string) error {
	switch suf {
	case "":
		return nil

	case "n":
		return mulPow10(r, -9)
	case "u":
		return mulPow10(r, -6)
	case "m":
		return mulPow10(r, -3)
	case "k":
		return mulPow10(r, 3)
	case "M":
		return mulPow10(r, 6)
	case "G":
		return mulPow10(r, 9)
	case "T":
		return mulPow10(r, 12)
	case "P":
		return mulPow10(r, 15)
	case "E":
		return mulPow10(r, 18)

	case "Ki":
		return mulPow2(r, 10)
	case "Mi":
		return mulPow2(r, 20)
	case "Gi":
		return mulPow2(r, 30)
	case "Ti":
		return mulPow2(r, 40)
	case "Pi":
		return mulPow2(r, 50)
	case "Ei":
		return mulPow2(r, 60)

	default:
		return fmt.Errorf("unknown suffix %q", suf)
	}
}

func mulPow10(r *big.Rat, exp int) error {
	if exp == 0 {
		return nil
	}
	if exp > 0 {
		r.Mul(r, new(big.Rat).SetInt(pow10Int(exp)))
		return nil
	}
	r.Quo(r, new(big.Rat).SetInt(pow10Int(-exp)))
	return nil
}

func mulPow2(r *big.Rat, bits int) error {
	if bits < 0 {
		return fmt.Errorf("invalid binary exponent")
	}
	m := new(big.Int).Lsh(big.NewInt(1), uint(bits))
	r.Mul(r, new(big.Rat).SetInt(m))
	return nil
}

func pow10Int(n int) *big.Int {
	if n < 0 {
		n = -n
	}
	ten := big.NewInt(10)
	out := big.NewInt(1)
	for i := 0; i < n; i++ {
		out.Mul(out, ten)
	}
	return out
}

func ratCeilToUint64(r *big.Rat) (uint64, error) {
	if r.Sign() < 0 {
		return 0, fmt.Errorf("negative value")
	}
	num := new(big.Int).Set(r.Num())
	den := new(big.Int).Set(r.Denom())

	q, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}

	if q.Sign() < 0 || q.BitLen() > 64 {
		return 0, fmt.Errorf("overflow")
	}
	return q.Uint64(), nil
}

// u64FromNonNegI64 converts int64 to uint64 without a direct cast (avoids gosec G115).
// Returns (0,false) for negative values or conversion errors.
func u64FromNonNegI64(v int64) (uint64, bool) {
	if v < 0 {
		return 0, false
	}
	u, err := strconv.ParseUint(strconv.FormatInt(v, 10), 10, 64)
	if err != nil {
		return 0, false
	}
	return u, true
}
