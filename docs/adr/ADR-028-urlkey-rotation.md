# ADR-028 — UrlKey Secret Rotation (kid in URL, multi-key Signer)

## 상태
Accepted · 2026-04-26 · Season 3 Ep.4

## 맥락

ADR-007 의 `urlkey.Signer` 는 단일 secret 만 보유:

```go
type Signer struct {
    secret []byte  // env EDGE_URLKEY_SECRET
}
```

운영 시 secret 교체가 필요한 시점:
1. **유출 의심** — 즉시 rotate, 이전 secret 즉시 무효화
2. **정기 교체** — 90일/1년 정책
3. **서명 알고리즘 변경 (미래)** — kid 가 secret 뿐 아니라 algorithm 도 가리킬 수 있음 (현 ADR 범위 밖)

현재 단일 secret 으로는:
- Rotate = `EDGE_URLKEY_SECRET` env 변경 + edge restart
- Restart 직후, 옛 secret 으로 발급된 모든 presigned URL **즉시 401** (사용자 측면 파괴적)

표준 해결: **kid (key id) 를 URL 에 포함**, signer 가 multi-key store. 옛 kid 는 grace period 동안 verify 만 허용 (sign 안 함), 그 후 retire.

### 대안 검토

| 방식 | 장점 | 단점 |
|---|---|---|
| **In-Signer multi-key + bbolt 영속 (이번 ADR)** | edge 자족, ADR-007/027 패턴 일관 | multi-edge 시 kid 동기화 (ADR-022 후보) |
| External KMS / Vault | 표준 | 외부 의존 +1 |
| dual env `EDGE_URLKEY_SECRET_OLD` + `EDGE_URLKEY_SECRET` | 코드 변경 최소 | 2개 이상 동시 보관 어려움, env 회전 비교란 |
| Signing-time embed plaintext kid name only (no rotation) | 코드 변경 0 | rotation 본질 미해결 |

ADR-027 의 dynamic DN registry 와 동일 패턴 (in-edge + bbolt) 채택.

## 결정

### 핵심 모델

```go
type Signer struct {
    mu      sync.RWMutex
    keys    map[string][]byte  // kid → secret
    primary string             // 새 Sign 호출이 사용할 kid
}

func (s *Signer) Sign(method, path string, exp int64) (sig, kid string)
func (s *Signer) BuildURL(method, path string, ttl time.Duration) string  // ?kid=...&sig=...&exp=...
func (s *Signer) Verify(method, path string, q url.Values, now time.Time) error
    // kid 없으면 primary 로 검증 (backward compat); 있으면 해당 kid 의 secret 사용

func (s *Signer) Add(kid string, secret []byte) error  // duplicate kid 거부
func (s *Signer) SetPrimary(kid string) error          // 미존재 거부
func (s *Signer) Remove(kid string) error              // primary 거부, 마지막 1개 거부
func (s *Signer) Kids() []string                       // 정렬된 list
```

### URL 형식

이전: `<path>?sig=<hex>&exp=<unix>`

이후: `<path>?kid=<id>&sig=<hex>&exp=<unix>`

`kid` 누락 = primary 로 검증 (옛 클라이언트 호환). 새로 발급되는 URL 은 항상 kid 포함.

**서명 입력은 그대로** (`<METHOD>:<PATH>:<EXP>`) — kid 는 key 선택용일 뿐 입력에 포함 안 함. Bind 강도가 약하지만 (kid swap 시도는 secret 모르면 의미 없음 — sig 가 새 kid 로 verify 통과 못 함) MVP 에 충분.

### bbolt 스키마

`urlkey_secrets` 버킷:

| key | value |
|---|---|
| `<kid>` | JSON `{secret_hex, created_at, is_primary}` |

Boot 시퀀스:
1. 버킷 비어있으면 → `EDGE_URLKEY_SECRET` 으로 kid `"v1"` seed (primary=true)
2. 비어있지 않으면 → 모두 로드, `is_primary=true` 인 kid 가 primary
3. `EDGE_URLKEY_PRIMARY_KID` env override 가능 (특정 kid 강제 primary)

### Admin endpoints

```
GET    /v1/admin/urlkey                  → {kids:[{id,created_at,is_primary}], primary:"v2"}
POST   /v1/admin/urlkey/rotate           body {"kid":"v2","secret":"<hex>"}
                                         → adds, sets new primary
DELETE /v1/admin/urlkey?kid=v1           → removes (primary 거부, 마지막 1개 거부)
```

`POST /rotate` 가 atomic: secret 추가 + primary 변경. CLI 가 random secret 생성.

### CLI

```
kvfs-cli urlkey list                     # 현재 kids 목록
kvfs-cli urlkey rotate                   # 자동 random secret 생성, primary 교체
kvfs-cli urlkey rotate --kid v3 --secret <hex>   # 명시
kvfs-cli urlkey remove --kid v1          # 옛 kid 삭제 (sign 안 받지만 verify 했던 것)
```

### 명시적 비범위

- **자동 rotation 정책 (90 days)** — 시간 기반 cron 은 ADR-013 보강 시 추가
- **algorithm rotation** — kid 가 algorithm 도 가리키는 모델 (예: HMAC-SHA256 → Ed25519). 현재 algorithm 1개. 별도 ADR
- **multi-edge kid 동기화** — bbolt 1 개 edge 가정. ADR-022 후속
- **secret 메모리 보호** — process memory 에 평문 secret. mlock / OS-level 보호 미지원
- **revoke (회수된 URL 무효화)** — exp 기반 자연 만료 외 별도 revocation list 없음

## 결과

### 긍정

- **운영자 1줄 명령으로 secret rotate** — `kvfs-cli urlkey rotate`
- **이전 발급 URL grace period** — 옛 kid retain 시 verify 통과
- **Backward compat** — kid 없는 URL 은 primary 로 verify (단일-secret 클라이언트 그대로 작동)
- **bbolt 영속** — restart 후 모든 kid 유지
- **ADR-007 minimal extension** — 알고리즘 + URL 형식 거의 그대로

### 부정

- **kid swap 공격은 막지 못함** — kid 를 입력에 포함 안 함. 이론적: 옛 kid `v1` 의 sig 를 `v2` 와 함께 보내면 verify 실패. 즉 실제 위험 없음. 그러나 입력 binding 이 약함 (best practice 면 kid 도 input)
- **multi-edge 미동기화** — 한 edge 에서 rotate 시 다른 edge 는 모름
- **secret 평문 저장** — bbolt 파일 권한 0600 이지만, secret 보호 강화 필요 시 OS keystore 통합 (별도 ADR)

### 트레이드오프 인정

- "왜 kid 를 input 에 포함 안 함?" — 입력 형식 변경 = 모든 발급 URL invalidate. backward compat 우선. 보안 분석상 약간의 binding 약함 수용
- "왜 자동 rotation 안 함?" — 정책 부담 (운영자가 모니터링 책임). 우선 수동 + 표시 → 후속 자동화

## 데모 시나리오

```
1. ./scripts/up.sh                                       # kid="v1" auto-seeded
2. ./bin/kvfs-cli urlkey list                            # [v1*]
3. PUT object → URL has ?kid=v1&sig=...
4. ./bin/kvfs-cli urlkey rotate                          # adds v2, primary=v2
5. ./bin/kvfs-cli urlkey list                            # [v1, v2*]
6. 새 PUT → URL has ?kid=v2&sig=...
7. 옛 v1 URL 도 여전히 GET 통과 (v1 secret 보존)
8. ./bin/kvfs-cli urlkey remove --kid v1                 # 옛 secret 회수
9. v1 URL → 401 unauthorized
```

## 관련

- ADR-007 (UrlKey HMAC-SHA256) — 이 ADR 의 supersede 가 아니라 minimal extension
- ADR-027 (Dynamic DN registry) — 동일 admin pattern
- `internal/urlkey/` — multi-key Signer
- `internal/store/` — `urlkey_secrets` bucket
- `internal/edge/` — `/v1/admin/urlkey/*` endpoints
- `cmd/kvfs-cli/` — `urlkey` subcommand

## 후속 ADR 예상

- **ADR-031 — Algorithm rotation**: kid → (algorithm, secret) tuple. HMAC-SHA256 → Ed25519 / SLH-DSA 등
- **ADR-032 — Auto-rotation policy**: time-based + alert
- **ADR-022 — Multi-edge sync**: kid registry 도 묶임
