package utils

import (
	"testing"
)

func TestClampUint32ToUint16(t *testing.T) {
	tests := []struct {
		input    uint32
		expected uint16
	}{
		{0, 0},
		{1000, 1000},
		{65535, 65535},
		{65536, 65535},  // clamp
		{100000, 65535}, // clamp
	}

	for _, tt := range tests {
		result := ClampUint32ToUint16(tt.input)
		if result != tt.expected {
			t.Errorf("ClampUint32ToUint16(%d) = %d; want %d", tt.input, result, tt.expected)
		}
	}
}

func TestSafeIntToUint16(t *testing.T) {
	tests := []struct {
		input    int
		expected uint16
	}{
		{0, 0},
		{1000, 1000},
		{65535, 65535},
		{65536, 65535}, // clamp
		{-1, 0},        // clamp
		{-100, 0},      // clamp
	}

	for _, tt := range tests {
		result := SafeIntToUint16(tt.input)
		if result != tt.expected {
			t.Errorf("SafeIntToUint16(%d) = %d; want %d", tt.input, result, tt.expected)
		}
	}
}

func TestSafeIntToUint64(t *testing.T) {
	tests := []struct {
		input    int
		expected uint64
	}{
		{0, 0},
		{1000, 1000},
		{2147483647, 2147483647},
		{-1, 0},   // clamp
		{-100, 0}, // clamp
	}

	for _, tt := range tests {
		result := SafeIntToUint64(tt.input)
		if result != tt.expected {
			t.Errorf("SafeIntToUint64(%d) = %d; want %d", tt.input, result, tt.expected)
		}
	}
}

func TestSafeIntToUint32(t *testing.T) {
	tests := []struct {
		input    int
		expected uint32
	}{
		{0, 0},
		{1000, 1000},
		{2147483647, 2147483647},
		{2147483648, 2147483647}, // clamp
		{-1, 0},                  // clamp
		{-100, 0},                // clamp
	}

	for _, tt := range tests {
		result := SafeIntToUint32(tt.input)
		if result != tt.expected {
			t.Errorf("SafeIntToUint32(%d) = %d; want %d", tt.input, result, tt.expected)
		}
	}
}

func TestSafeIntToInt32(t *testing.T) {
	tests := []struct {
		input    int
		expected int32
	}{
		{0, 0},
		{1000, 1000},
		{2147483647, 2147483647},
		{2147483648, 2147483647}, // clamp
		{-2147483648, -2147483648},
		{-2147483649, -2147483648}, // clamp
	}

	for _, tt := range tests {
		result := SafeIntToInt32(tt.input)
		if result != tt.expected {
			t.Errorf("SafeIntToInt32(%d) = %d; want %d", tt.input, result, tt.expected)
		}
	}
}
