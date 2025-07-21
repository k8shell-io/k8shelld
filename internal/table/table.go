package table

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// FieldType represents the type of a field in a table.
type FieldType int

// FieldType constants
const (
	String FieldType = iota
	Int
	Float
	Time
)

// ColSpacing is the number of spaces between columns.
const ColSpacing = 2

// FieldDefinition represents a field in a table.
type FieldDefinition struct {
	Name        string
	Width       int
	Type        FieldType
	Format      string // Used for time formatting
	DisplayFunc func(row map[string]interface{}, name string) string
}

// Table represents a table with fields and data.
type Table struct {
	fields    []FieldDefinition
	data      []map[string]interface{}
	colorFunc func(map[string]interface{}) string
}

// isTerminal checks if the given file descriptor refers to a terminal.
func isTerminal(fd uintptr) bool {
	var ws struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	}
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	return err == 0
}

// getTerminalWidth retrieves the terminal width from the environment or defaults to 80.
func getTerminalWidth() (int, error) {
	// Check if the COLUMNS environment variable is set
	if cols, ok := os.LookupEnv("COLUMNS"); ok {
		if width, err := strconv.Atoi(cols); err == nil && width > 0 {
			return width, nil
		}
	}

	if isTerminal(os.Stdout.Fd()) {
		var ws struct {
			Row    uint16
			Col    uint16
			Xpixel uint16
			Ypixel uint16
		}
		_, _, err := syscall.Syscall(syscall.SYS_IOCTL, uintptr(os.Stdout.Fd()),
			uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
		if err == 0 {
			return int(ws.Col), nil
		}
	}

	return 80, nil
}

// NewTable creates a new table with the specified fields and color function.
// The color function is used to color rows based on their content, it should return an ANSI color code.
func NewTable(fields []FieldDefinition, colorFunc func(map[string]interface{}) string) *Table {
	return &Table{
		fields:    fields,
		data:      nil,
		colorFunc: colorFunc,
	}
}

// Parse processes the data to conform to field definitions.
// The data should be a slice of maps where each map represents a row.
func (t *Table) ParseJsonData(dataJson []byte) error {
	// Parse the JSON data
	var data []map[string]interface{}
	if err := json.Unmarshal(dataJson, &data); err != nil {
		return fmt.Errorf("failed to parse JSON data: %v", err)
	}

	for _, row := range data {
		for _, field := range t.fields {
			value, exists := row[field.Name]
			if !exists {
				row[field.Name] = "-"
				continue
			}

			// Parse the field based on its type
			switch field.Type {
			case String:
				if _, ok := value.(string); !ok {
					row[field.Name] = fmt.Sprintf("%v", value)
				}
			case Int:
				if v, ok := value.(float64); ok {
					row[field.Name] = int(v)
				} else if _, ok := value.(int); !ok {
					row[field.Name] = 0
				}
			case Float:
				if _, ok := value.(float64); !ok {
					row[field.Name] = 0.0
				}
			case Time:
				if str, ok := value.(string); ok {
					format := field.Format
					if format == "" {
						format = time.RFC3339
					}
					val, err := time.Parse(format, str)
					if err != nil {
						row[field.Name] = "-"
					} else {
						row[field.Name] = val
					}
				} else {
					row[field.Name] = "-"
				}
			}
		}
	}

	t.data = data
	return nil
}

// Sort sorts the table data by the specified fields.
// The `sortFields` argument is a slice of field names with an optional prefix "-" for descending order.
func (t *Table) Sort(sortFields []string) error {
	// check all fields are valid
	for _, field := range sortFields {
		found := false
		for _, f := range t.fields {
			if strings.TrimPrefix(field, "-") == f.Name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("invalid field: %s", field)
		}
	}

	// Sort the data
	sort.Slice(t.data, func(i, j int) bool {
		for _, field := range sortFields {
			desc := false
			if strings.HasPrefix(field, "-") {
				desc = true
				field = strings.TrimPrefix(field, "-")
			}

			valI, okI := t.data[i][field]
			valJ, okJ := t.data[j][field]
			if !okI || !okJ {
				return okI // Handle missing fields by treating them as less significant
			}

			switch vI := valI.(type) {
			case string:
				vJ, ok := valJ.(string)
				if !ok {
					continue
				}
				if vI == vJ {
					continue
				}
				if desc {
					return vI > vJ
				}
				return vI < vJ
			case float64:
				vJ, ok := valJ.(float64)
				if !ok {
					continue
				}
				if vI == vJ {
					continue
				}
				if desc {
					return vI > vJ
				}
				return vI < vJ
			case int:
				vJ, ok := valJ.(int)
				if !ok {
					continue
				}
				if vI == vJ {
					continue
				}
				if desc {
					return vI > vJ
				}
				return vI < vJ
			case time.Time:
				vJ, ok := valJ.(time.Time)
				if !ok {
					continue
				}
				if vI.Equal(vJ) {
					continue
				}
				if desc {
					return vI.After(vJ)
				}
				return vI.Before(vJ)
			}

			// Default comparison for unsupported types
			strI := fmt.Sprintf("%v", valI)
			strJ := fmt.Sprintf("%v", valJ)
			if strI == strJ {
				continue
			}
			if desc {
				return strI > strJ
			}
			return strI < strJ
		}
		return false
	})
	return nil
}

// DisplayTable displays a JSON array as a table, trimming rows to fit terminal width.
func (t *Table) DisplayTable() error {
	terminalWidth, err := getTerminalWidth()
	if err != nil {
		return fmt.Errorf("failed to get terminal width: %v", err)
	}

	// Print header
	rowWidth := 0
	for i, field := range t.fields {
		width := field.Width + ColSpacing
		column := fmt.Sprintf("%-*s", width, strings.ToUpper(field.Name))
		if rowWidth+len(column) > terminalWidth {
			if i < len(t.fields)-1 {
				fmt.Print(column[:terminalWidth-rowWidth-3] + "...")
			} else {
				fmt.Print(column[:terminalWidth-rowWidth])
			}
			break
		}
		rowWidth += len(column)
		fmt.Print(column)
	}
	fmt.Println()

	// Print rows
	for _, row := range t.data {
		rowWidth = 0

		var rowColor string
		if t.colorFunc != nil {
			rowColor = t.colorFunc(row)
		} else {
			rowColor = ""
		}

		for i, field := range t.fields {
			var value string
			if field.DisplayFunc != nil {
				value = field.DisplayFunc(row, field.Name)
			} else {
				value = fmt.Sprintf("%v", row[field.Name])
			}

			width := field.Width + ColSpacing
			if len(value) > width {
				value = value[:width-3] + "..."
			}
			if value == "" {
				value = "-"
			}

			column := fmt.Sprintf("%-*s", width, value)
			if rowWidth+len(column) > terminalWidth {
				if i < len(t.fields)-1 {
					column = column[:terminalWidth-rowWidth-3] + "..."
				} else {
					column = column[:terminalWidth-rowWidth]
				}
				if rowColor != "" {
					fmt.Printf("%s%s%s", rowColor, column, "\033[0m")
				} else {
					fmt.Print(column)
				}
				break
			}
			if rowColor != "" {
				fmt.Printf("%s%s%s", rowColor, column, "\033[0m")
			} else {
				fmt.Print(column)
			}
			rowWidth += len(column)
		}
		fmt.Println()
	}

	return nil
}

// DisplayJSON outputs the table's data as an indented JSON string.
func (t *Table) DisplayJSON() error {
	indentedJSON, err := json.MarshalIndent(t.data, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %v", err)
	}

	fmt.Println(string(indentedJSON))
	return nil
}
