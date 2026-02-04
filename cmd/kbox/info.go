package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

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

		startTime, err := time.Parse(time.RFC3339, system.Uptime)
		if err != nil {
			fmt.Println("Error parsing uptime:", err)
			return
		}
		workspace := [][2]string{
			{"Name", env("WORKSPACE", "n/a")},
			{"Start time", startTime.Format("2006-01-02 15:04:05")},
			{"Image", env("IMAGE", "n/a")},
			{"Blueprint", env("BLUEPRINT", "n/a")},
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

			totalLine := formatBytesIEC(du.TotalBytes, 0)
			if du.DeclaredSize > 0 {
				totalLine = fmt.Sprintf(
					"%s / %s (%s)",
					formatBytesIEC(du.TotalBytes, 0),
					formatBytesIEC(du.DeclaredSize, 0),
					pct(float64(du.TotalBytes), float64(du.DeclaredSize)),
				)
			}

			dockerLines := [][2]string{
				{"Socket", du.SocketPath},
				{"API version", du.APIVersion},
				{"Images", formatBytesIEC(du.ImagesBytes, 0)},
				{"Containers (rw)", formatBytesIEC(du.ContainersBytes, 0)},
				{"Containers (rootfs)", formatBytesIEC(du.ContainersRootFsBytes, 0)},
				{"Volumes", formatBytesIEC(du.VolumesBytes, 0)},
				{"Build cache", formatBytesIEC(du.BuildCacheBytes, 0)},
				{"Total", totalLine},
			}
			printGroup("Docker", dockerLines)
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
