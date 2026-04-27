// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package httputil

import (
	"fmt"
	"net/url"
	"strconv"
)

// ParseNonNegIntQuery extracts a non-negative integer query param.
// Returns (0, nil) when the param is absent — callers use the default.
// Returns ("", err) on bad input — caller maps the error to 400.
//
// `max == 0` disables the upper bound. Callers that have an explicit
// ceiling (replicationFactor, MaxRepairs vs payload size) pass it.
//
// Background: ADR-053/059 introduced four+ HTTP query params shaped
// the same way (max_repairs, concurrency, X-KVFS-W, X-KVFS-R) and the
// P8-12 simplify reviewer flagged the recurring inline parse pattern.
// This is the extraction.
func ParseNonNegIntQuery(q url.Values, name string, max int) (int, error) {
	v := q.Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: not an integer: %w", name, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s=%d: must be non-negative", name, n)
	}
	if max > 0 && n > max {
		return 0, fmt.Errorf("%s=%d: exceeds upper bound %d", name, n, max)
	}
	return n, nil
}
