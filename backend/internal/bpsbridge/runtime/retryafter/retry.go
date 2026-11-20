// Package retryafter parses bounded, credential-free retry hints.
package retryafter

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func Seconds(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		t, e := http.ParseTime(value)
		if e != nil {
			return 0
		}
		n = int(time.Until(t).Seconds()) + 1
	}
	if n < 1 {
		return 0
	}
	if n > 604800 {
		return 604800
	}
	return n
}
