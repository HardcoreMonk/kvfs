// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 The kvfs Authors. Licensed under the Apache License, Version 2.0.

// Package httputil holds shared HTTP-handler helpers used across kvfs's
// daemons (edge, coord, dn, future). Currently: JSON write + error write.
//
// 비전공자용 해설
// ──────────────
// edge 와 coord 가 같은 모양의 writeJSON / writeErr 를 각자 들고 있다.
// dn 도 일부 admin endpoint 가 비슷한 모양. 패키지로 빼서 단일 출처.
//
// helper 의 contract 는 단순:
//   WriteJSON  → Content-Type: application/json + status + Encode(body)
//   WriteError → WriteJSON 의 wrapper: body = {"error": msg}
//
// Encode 의 error 는 swallow — 클라이언트가 이미 끊었을 수도 있고, 응답
// 헤더는 이미 보낸 후라 정정 불가. 모든 callers 도 그렇게 했음.
package httputil

import (
	"encoding/json"
	"net/http"
)

// WriteJSON sets Content-Type, writes status, and encodes body. Used by
// every kvfs HTTP handler that returns JSON.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteError is the conventional shape: {"error": "<msg>"} with the
// given status code. Mirrors edge's prior writeError exactly.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// WriteErr is the error-typed variant — convenient when the caller has
// an error in hand. Same shape as WriteError after err.Error().
func WriteErr(w http.ResponseWriter, status int, err error) {
	WriteError(w, status, err.Error())
}
