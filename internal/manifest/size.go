package manifest

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// sizePattern is a number, a unit letter, and the suffixes that may follow it:
// "512M", "512MB", "512Mi", "512MiB", "1.5g". A number without a unit is not a
// size, because it does not say whether it is bytes or megabytes.
var sizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([kmgt])(i?b?)$`)

// ParseSize reads a size such as "512M" or "1.5Gi" as a number of bytes. The
// unit is K, M, G or T in either case, optionally followed by B, i or iB, and
// all of them are powers of 1024, as the MEM column writes them. A size that
// rounds to no bytes, and a bare 0, is 0, which the caller decides what to
// make of.
func ParseSize(s string) (int64, error) {
	// Zero needs no unit to be unambiguous, and "max-memory: 0" is what
	// someone writes who means no limit.
	if strings.TrimSpace(s) == "0" {
		return 0, nil
	}
	m := sizePattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("%q is not a size with a unit, such as 512M or 2G", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a size: %v", s, err)
	}

	unit := int64(1) << (10 * (1 + strings.Index("kmgt", m[2])))
	bytes := math.Round(n * float64(unit))
	if bytes >= math.MaxInt64 { // as a float, MaxInt64 is 2^63, which is already too big
		return 0, fmt.Errorf("%q is too large a size", s)
	}
	return int64(bytes), nil
}
