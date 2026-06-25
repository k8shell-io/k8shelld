// Use of this source code is governed by a AGPLv3
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/k8shell-io/common/pkg/api/client/k8shelld"
	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/spf13/cobra"
)

var infoJSON bool

func init() {
	InfoCmd.Flags().BoolVar(&infoJSON, "json", false, "Output JSON (pretty-printed)")
}

var InfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Display workspace system info",
	Long: `Display workspace, CPU/memory, storage mounts, and Podman usage information.

Workspace:
  - Name: workspace name
  - Start time: workspace start time
  - Provisioner: provisioner version
  - k8shelld: server version
  - Image: workspace image reference
  - Blueprint: workspace blueprint
  - Repository: linked source repository
  - Users: active sessions

CPU and Memory:
  - CPU usage: used millicores / limit millicores (%)
  - Load average: 1m, 5m, 15m
  - Memory usage: used / limit (%)

Storage:
  - Per mount: used / limit (%), plus fs type and source when available

Podman (if available):
  - Socket, API version, Podman version, graph driver, graph root, run root
  - Container counts (total / running / paused / stopped)
  - Images, containers (rw/rootfs), volumes, build cache
  - Total: used / limit (%)`,
	Run: func(cmd *cobra.Command, args []string) {
		if infoJSON {
			raw, err := fetchSysInfoRaw()
			if err != nil {
				fmt.Println(err.Error())
				return
			}
			var buf bytes.Buffer
			if err := json.Indent(&buf, raw, "", "  "); err != nil {
				fmt.Println(string(raw))
				return
			}
			fmt.Println(buf.String())
			return
		}

		sysInfo, err := fetchSysInfo()
		if err != nil {
			fmt.Println(err.Error())
			return
		}

		system := sysInfo.System

		startTime, err := time.Parse(time.RFC3339, system.Uptime)
		if err != nil {
			fmt.Println("Error parsing uptime:", err)
			return
		}

		repoName := sysInfo.Repository
		if repoName == "" {
			repoName = "n/a"
		}

		k8shelldVersion := sysInfo.Version
		if k8shelldVersion == "" {
			k8shelldVersion = "n/a"
		}

		workspace := [][2]string{
			{"Name", env("WORKSPACE", "n/a")},
			{"Start time", startTime.Local().Format("2006-01-02 15:04:05 MST")},
			{"Provisioner", env("PROVISIONER_VERSION", "n/a")},
			{"k8shelld", k8shelldVersion},
			{"Image", env("IMAGE", "n/a")},
			{"Blueprint", env("BLUEPRINT", "n/a")},
			{"Repository", repoName},
			{"Users", fmt.Sprintf("%d", system.Users)},
		}
		printGroup("Workspace", workspace)

		cpuMem := [][2]string{
			{"CPU usage", fmt.Sprintf("%.2fm / %.0fm (%s)", system.CPUUsageMillicores, system.CPULimitMillicores,
				pct(system.CPUUsageMillicores, system.CPULimitMillicores))},
			{"Load average", fmt.Sprintf("%.2f, %.2f, %.2f", system.CPUAvg1Min,
				system.CPUAvg5Min, system.CPUAvg15Min)},
			{"Memory usage", fmt.Sprintf("%.2fMiB / %.0fMiB (%s)", system.MemoryUsageMiB, system.MemLimitMiB,
				pct(system.MemoryUsageMiB, system.MemLimitMiB))},
		}
		printGroup("CPU and Memory", cpuMem)

		mounts := sysInfo.Mounts
		sort.Slice(mounts, func(i, j int) bool { return mounts[i].MountPoint < mounts[j].MountPoint })

		stLines := make([][2]string, 0, len(mounts))
		for _, m := range mounts {
			total := formatBytesIEC(m.TotalBytes, 0)
			used := formatBytesIEC(m.UsedBytes, 0)

			extra := []string{}
			if m.FSType != "" {
				extra = append(extra, "fs="+m.FSType)
			}
			if m.Source != "" {
				extra = append(extra, "src="+m.Source)
			}
			if m.ReadOnly {
				extra = append(extra, "ro")
			}

			// Only percent; do not repeat "used" / "avail" phrases.
			val := fmt.Sprintf("%s / %s (%s)", used, total, pct(float64(m.UsedBytes), float64(m.TotalBytes)))
			if len(extra) > 0 {
				val += "  [" + strings.Join(extra, ", ") + "]"
			}

			stLines = append(stLines, [2]string{m.MountPoint, val})
		}
		printGroup("Storage", stLines)

		if sysInfo.Docker != nil {
			du := sysInfo.Docker
			pd := sysInfo.Podman

			totalLine := formatBytesIEC(du.TotalBytes, 0)
			if du.DeclaredSize > 0 {
				totalLine = fmt.Sprintf(
					"%s / %s (%s)",
					formatBytesIEC(du.TotalBytes, 0),
					formatBytesIEC(du.DeclaredSize, 0),
					pct(float64(du.TotalBytes), float64(du.DeclaredSize)),
				)
			}

			apiVersionLine := du.APIVersion
			if pd != nil && pd.PodmanVersion != "" {
				apiVersionLine = fmt.Sprintf("%s (Podman %s)", du.APIVersion, pd.PodmanVersion)
			}

			podmanLines := [][2]string{
				{"Socket", du.SocketPath},
				{"API version", apiVersionLine},
			}
			if pd != nil {
				if pd.GraphDriver != "" {
					podmanLines = append(podmanLines, [2]string{"Graph driver", pd.GraphDriver})
				}
				if du.DockerRootDir != "" {
					podmanLines = append(podmanLines, [2]string{"Graph root", du.DockerRootDir})
				}
				if pd.RunRoot != "" {
					podmanLines = append(podmanLines, [2]string{"Run root", pd.RunRoot})
				}
				containersLine := fmt.Sprintf("%d total (%d running", pd.ContainersTotal, pd.ContainersRunning)
				if pd.ContainersPaused > 0 {
					containersLine += fmt.Sprintf(", %d paused", pd.ContainersPaused)
				}
				containersLine += fmt.Sprintf(", %d stopped)", pd.ContainersStopped)
				podmanLines = append(podmanLines, [2]string{"Containers", containersLine})
			}
			podmanLines = append(podmanLines,
				[2]string{"Images", formatBytesIEC(du.ImagesBytes, 0)},
				[2]string{"Containers (rw)", formatBytesIEC(du.ContainersBytes, 0)},
				[2]string{"Containers (rootfs)", formatBytesIEC(du.ContainersRootFsBytes, 0)},
				[2]string{"Volumes", formatBytesIEC(du.VolumesBytes, 0)},
				[2]string{"Build cache", formatBytesIEC(du.BuildCacheBytes, 0)},
				[2]string{"Total", totalLine},
			)
			printGroup("Podman", podmanLines)
		}
	},
}

// sysInfoResp extends k8shelld.SystemInfo with extra Podman-specific fields
// that are returned by the REST /sysinfo endpoint but have no proto equivalent.
type sysInfoResp struct {
	k8shelld.SystemInfo
	Podman  *podmanExt `json:"podman,omitempty"`
	Version string     `json:"version"`
}

type podmanExt struct {
	PodmanVersion     string `json:"podmanVersion"`
	GraphDriver       string `json:"graphDriver"`
	RunRoot           string `json:"runRoot"`
	ContainersTotal   int    `json:"containersTotal"`
	ContainersRunning int    `json:"containersRunning"`
	ContainersPaused  int    `json:"containersPaused"`
	ContainersStopped int    `json:"containersStopped"`
}

func fetchSysInfo() (*sysInfoResp, error) {
	resp, err := client.MakeRequest("GET", "/sysinfo", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("error fetching sysinfo: %w", err)
	}
	defer resp.Body.Close()

	if err := client.CheckApplicationError(resp); err != nil {
		return nil, err
	}

	var data sysInfoResp
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("error parsing sysinfo response: %w", err)
	}
	return &data, nil
}

func fetchSysInfoRaw() ([]byte, error) {
	resp, err := client.MakeRequest("GET", "/sysinfo", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("error fetching sysinfo: %w", err)
	}
	defer resp.Body.Close()

	if err := client.CheckApplicationError(resp); err != nil {
		return nil, err
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading sysinfo response: %w", err)
	}
	return b, nil
}

func printGroup(title string, rows [][2]string) {
	fmt.Println(title)
	if len(rows) == 0 {
		fmt.Println()
		return
	}

	// Use tabwriter to guarantee alignment (fixes occasional misalignment).
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintf(tw, "  %s:\t%s\n", r[0], r[1])
	}
	_ = tw.Flush()
	fmt.Println()
}

func pct(used, total float64) string {
	if total == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f%%", (used/total)*100.0)
}

func formatBytesIEC(b uint64, dec int) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
		TiB = 1024 * GiB
	)

	switch {
	case b >= TiB:
		return fmt.Sprintf("%.*fTiB", dec, float64(b)/float64(TiB))
	case b >= GiB:
		return fmt.Sprintf("%.*fGiB", dec, float64(b)/float64(GiB))
	case b >= MiB:
		return fmt.Sprintf("%.*fMiB", dec, float64(b)/float64(MiB))
	case b >= KiB:
		return fmt.Sprintf("%.*fKiB", dec, float64(b)/float64(KiB))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

func env(envKey string, fallback string) string {
	v := strings.TrimSpace(os.Getenv(envKey))
	if v == "" {
		return fallback
	}
	return v
}
