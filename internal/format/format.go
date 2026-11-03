// Package format renders the values the interface and the log show to a
// person.
package format

import "fmt"

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
