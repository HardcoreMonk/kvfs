// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package cliutil holds the four mini-helpers that every kvfs `cmd/<bin>/
// main.go` needs verbatim: env lookup with default, ASCII-int parse with
// default, comma-split with trim, and a stderr-prefixed fatal exit.
//
// 비전공자용 해설
// ──────────────
// 4 개 daemon 의 main.go 가 자기 envOr/atoiOr/splitCSV/fatal 을 들고 있다.
// 같은 코드 4 카피 — 추가/수정 시 4번 같은 일. 작은 패키지로 모은다.
//
// fatal 의 prefix 는 caller 가 정함 (e.g. "kvfs-edge: ") 이므로 함수가
// prefix 를 받지 않고 caller 가 미리 string 만들어 호출.
package cliutil

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// EnvOr returns os.LookupEnv(k) if present (including empty string),
// otherwise def. Mirrors the per-daemon envOr the project has used since
// Season 1.
//
// Empty-set vs unset: this returns "" when the operator sets `KEY=`
// explicitly (LookupEnv treats it as set). Most kvfs callers compare to
// a non-empty constant ("1", "cdc"), so empty acts like unset there.
// Edge case: env vars whose default is a non-empty Go duration string
// (e.g., "5m") get fatal errors at time.ParseDuration("") if the
// operator clears them — that's louder than the prior os.Getenv-based
// helpers (which would have silently used the default). Acceptable —
// fail-loud > silently-default for misconfig.
func EnvOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

// AtoiOr parses s as int. On any error returns def. Use for env vars
// that should be numeric but with a sane default.
func AtoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// SplitCSV trims and de-duplicates whitespace from a comma-separated
// list, dropping empties. "a, b,, c" → ["a","b","c"].
func SplitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Fatal writes msg to stderr and exits with code 2. Caller embeds its
// own prefix (e.g., `cliutil.Fatal("kvfs-edge: " + err.Error())`).
func Fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}
