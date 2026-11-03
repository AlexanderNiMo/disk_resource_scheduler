// Package format renders the values the interface and the log show to a
// person.
package format

import (
	"fmt"
	"time"
)

// Bytes renders a file size the way a person reads it.
//
// The raw byte count is what the plan holds and what a script wants; the
// page and the log are read by people.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}

	value := float64(n)
	for _, u := range []string{"КиБ", "МиБ", "ГиБ", "ТиБ"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f ПиБ", value)
}

// Duration renders a span the way a person reads it: "24 ч", "1 ч 30 мин".
func Duration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		days := int(d / (24 * time.Hour))
		if days%24 == 0 {
			return fmt.Sprintf("%d сут", days/24)
		}
		return fmt.Sprintf("%d ч", int(d/time.Hour))
	case d >= time.Hour:
		return fmt.Sprintf("%d ч", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%d мин", int(d/time.Minute))
	default:
		return fmt.Sprintf("%d с", int(d/time.Second))
	}
}
