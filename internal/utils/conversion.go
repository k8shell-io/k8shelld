package utils

// ClampUint32ToUint16 safely converts uint32 to uint16, clamping to 65535 if overflow.
func ClampUint32ToUint16(v uint32) uint16 {
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}

// SafeIntToUint16 safely converts int to uint16, clamping to 0-65535.
func SafeIntToUint16(v int) uint16 {
	if v < 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}

// SafeIntToUint64 converts int to uint64, assuming non-negative input (e.g., from Read()).
func SafeIntToUint64(v int) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// SafeIntToUint32 converts int to uint32, clamping negative values to 0.
func SafeIntToUint32(v int) uint32 {
	if v < 0 {
		return 0
	}
	if v > 2147483647 {
		return 2147483647
	}
	return uint32(v)
}

// SafeIntToInt32 converts int to int32, clamping to int32 max/min.
func SafeIntToInt32(v int) int32 {
	if v > 2147483647 {
		return 2147483647
	}
	if v < -2147483648 {
		return -2147483648
	}
	return int32(v)
}
