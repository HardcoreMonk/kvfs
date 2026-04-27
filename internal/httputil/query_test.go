// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

package httputil

import (
	"net/url"
	"testing"
)

func TestParseNonNegIntQuery(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		max     int
		want    int
		wantErr bool
	}{
		{"empty → 0 default", "", 10, 0, false},
		{"valid mid-range", "5", 10, 5, false},
		{"valid zero is OK", "0", 10, 0, false},
		{"valid at upper bound", "10", 10, 10, false},
		{"exceeds upper bound", "11", 10, 0, true},
		{"max=0 disables ceiling", "9999", 0, 9999, false},
		{"negative", "-1", 10, 0, true},
		{"non-numeric", "abc", 10, 0, true},
		{"empty value treated as missing", "x=", 10, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, _ := url.ParseQuery("x=" + tc.query)
			if tc.query == "" || tc.name == "empty value treated as missing" {
				q = url.Values{}
				if tc.name == "empty value treated as missing" {
					q.Set("x", "")
				}
			}
			got, err := ParseNonNegIntQuery(q, "x", tc.max)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got=%d want=%d", got, tc.want)
			}
		})
	}
}
