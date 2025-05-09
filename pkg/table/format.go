package table

import (
	"fmt"
	"time"
)

// DisplayDateTime converts a time.Time value to a string in time.Stamp format
func DisplayDateTime(row map[string]interface{}, field string) string {
	created, ok := row[field].(time.Time)
	if !ok {
		return "-"
	}
	return created.Format(time.Stamp)
}

// DisplayDuration calculates the duration between two time.Time values and returns a string
func DisplayDuration(row map[string]interface{}, field string) string {
	created, ok1 := row["created"].(time.Time)
	deleted, ok2 := row["deleted"].(time.Time)
	if !ok1 {
		return "-"
	}
	if !ok2 {
		deleted = time.Now()
	}
	duration := deleted.Sub(created)
	return duration.Truncate(time.Second).String()
}

// DisplayBytes converts an integer value to a string in bytes, kilobytes, megabytes, or gigabytes
func DisplayBytes(row map[string]interface{}, field string) string {
	bytes, ok := row[field].(int)
	if !ok {
		return "-"
	}
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.2fG", float64(bytes)/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.2fM", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.2fK", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}
