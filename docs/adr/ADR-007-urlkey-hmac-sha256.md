# ADR-007 — UrlKey = HMAC-SHA256

## 상태
Accepted · 2026-04-25

## 맥락

ε 데모 핵심 — presigned URL 패턴. URL 자체에 만료·권한 서명 포함, edge가 오리진 접근 없이 검증.

산업 표준:
- AWS S3 pre-signed URL — HMAC-SHA256 기반 SigV4
- Cloudflare signed URL — HMAC-SHA256
- JWT — RS256/ES256/HS256

대안 — APR1-MD5 + Base62 + LuaJIT int64 FFI 기반 서명:
- APR1-MD5는 MD5 기반 — 2026 기준 추가 선택 이유 없음
- LuaJIT `int64_t` FFI는 signed/unsigned 변환에서 플랫폼 차이 발생 가능
- Base62 encoding은 URL-safe 이점이었으나 hex 대비 이득 미미

## 결정

**HMAC-SHA256 + hex encoding**:

- 입력: `"<METHOD>:<PATH>:<EXP_UNIX>"`
- 출력: `hex(HMAC-SHA256(secret, input))` 64자
- URL: `<path>?sig=<hex>&exp=<unix-seconds>`
- secret: 16+ bytes 랜덤 (프로덕션), 데모는 `EDGE_URLKEY_SECRET` env 고정값
- 검증: `hmac.Equal([]byte(expected), []byte(got))` — 상수 시간 비교

기존 형식과의 **비트 호환 불필요** — kvfs는 clean slate (ADR-001).

## 결과

**긍정**:
- Go stdlib(`crypto/hmac` + `crypto/sha256` + `encoding/hex`) 만으로 전체 구현 ~90 LOC
- Timing attack 안전 (`hmac.Equal`)
- 2026 표준 기법과 정합 — S3·Cloudflare·GCS 모두 동일 패턴
- FFI·int64 bug 클래스 원천 차단

**부정**:
- URL 길이 ~100자 증가 (hex 64자 + exp ~10자 + 구분자) — 대부분 무해
- 서명 재발급 필요 (만료 시 self-service) — 클라이언트 복잡도 소폭 증가

**트레이드오프**:
- SigV4 (multi-header canonical request) 같은 완전한 S3 호환은 **Season 2 소재** — 현재는 단순 path+exp 서명으로 시작
- JWT 도입은 과함 — 토큰 payload 없이 만료만 검증하는 용도엔 HMAC 서명이 자연

## 보안 주의

- `EDGE_SKIP_AUTH=1` 은 데모 편의 용도만. 프로덕션·공개 배포 **금지** (README·코드·CLAUDE.md에 경고)
- secret rotation — 현재 단일 키. Season 2에 `kid` (key id) 지원해 다중 secret·로테이션 가능 (`bucket "urlkey_secrets"` 에 `kid → secret` 저장)

## 관련

- `internal/urlkey/urlkey.go` — Sign/Verify 구현
- `internal/urlkey/urlkey_test.go` — round-trip, tamper, expired, method mismatch 5 tests PASS
- `cmd/kvfs-cli/main.go` — `sign` 서브커맨드
