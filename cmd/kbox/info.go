package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/k8shell-io/k8shelld/internal/client"
	"github.com/k8shell-io/k8shelld/pkg/api"
	"github.com/spf13/cobra"
)

var InfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Display workspace system info",
	Long:  "Display workspace system info including CPU/memory, storage mounts, and Docker usage.",
	Run: func(cmd *cobra.Command, args []string) {
		sysInfo, err := fetchSysInfo()
		if err != nil {
			fmt.Println(err.Error())
			return
		}

		system := sysInfo.System

		workspace := [][2]string{
			{"Uptime (since)", system.Uptime},
			{"Users", fmt.Sprintf("%d", system.Users)},
		}
		printGroup("Workspace", workspace)

		cpuMem := [][2]string{
			{"CPU usage", fmt.Sprintf("%.2fm / %.2fm (%s)", system.CPUUsageMillicores, system.CPULimitMillicores,
				pct(system.CPUUsageMillicores, system.CPULimitMillicores))},
			{"Memory usage", fmt.Sprintf("%.2fMiB / %.2fMiB (%s)", system.MemoryUsageMiB, system.MemLimitMiB,
				pct(system.MemoryUsageMiB, system.MemLimitMiB))},
			{"Load average", fmt.Sprintf("%.2f, %.2f, %.2f", system.CPUAvg1Min,
				system.CPUAvg5Min, system.CPUAvg15Min)},
		}
		printGroup("CPU and Memory", cpuMem)

		mounts := sysInfo.Mounts
		sort.Slice(mounts, func(i, j int) bool { return mounts[i].MountPoint < mounts[j].MountPoint })

		stLines := make([][2]string, 0, len(mounts))
		for _, m := range mounts {
			total := formatBytesIEC(m.TotalBytes)
			used := formatBytesIEC(m.UsedBytes)

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
			dockerLines := [][2]string{
				{"Socket", du.SocketPath},
				{"API version", du.APIVersion},
				{"Root dir", du.DockerRootDir},
				{"Images", formatBytesIEC(du.ImagesBytes)},
				{"Containers (rw)", formatBytesIEC(du.ContainersBytes)},
				{"Containers (rootfs)", formatBytesIEC(du.ContainersRootFsBytes)},
				{"Volumes", formatBytesIEC(du.VolumesBytes)},
				{"Build cache", formatBytesIEC(du.BuildCacheBytes)},
				{"Total", formatBytesIEC(du.TotalBytes)},
			}
			printGroup("Docker", dockerLines)
		} else {
			printGroup("Docker", [][2]string{{"Status", "not available"}})
		}
	},
}

func fetchSysInfo() (*api.SystemInfo, error) {
	resp, err := client.MakeRequest("GET", "/sysinfo", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("error fetching sysinfo: %w", err)
	}
	defer resp.Body.Close()

	if err := client.CheckApplicationError(resp); err != nil {
		return nil, err
	}

	var data api.SystemInfo
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("error parsing sysinfo response: %w", err)
	}
	return &data, nil
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

func formatBytesIEC(b uint64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
		TiB = 1024 * GiB
	)

	switch {
	case b >= TiB:
		return fmt.Sprintf("%.2fTiB", float64(b)/float64(TiB))
	case b >= GiB:
		return fmt.Sprintf("%.2fGiB", float64(b)/float64(GiB))
	case b >= MiB:
		return fmt.Sprintf("%.2fMiB", float64(b)/float64(MiB))
	case b >= KiB:
		return fmt.Sprintf("%.2fKiB", float64(b)/float64(KiB))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
