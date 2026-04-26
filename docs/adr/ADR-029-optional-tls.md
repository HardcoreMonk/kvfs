# ADR-029 — Optional TLS / mTLS (env-driven, opt-in)

## 상태
Accepted · 2026-04-26 · Season 3 Ep.5

## 맥락

지금까지 모든 통신은 평문 HTTP:
- 클라이언트 → edge: `http://localhost:8000`
- edge → DN: `http://dn1:8080`
- DN ↔ 클러스터 내부: 신뢰

데모/교육 목적엔 적합하지만, 실 환경에서는 최소한:
1. **Edge 외부 통신 TLS** — 클라이언트가 인터넷에서 접근 시 sniff/MitM 방지
2. **Edge ↔ DN mTLS** — 같은 데이터센터 내부도 zero-trust 표준이 되어가는 추세

ADR-007 (UrlKey HMAC) 이 application-layer 인증을 제공하지만, 평문 채널이면 sig + body sniff 가능.

ADR-013 (auto-trigger), ADR-027 (dynamic DN), ADR-028 (urlkey rotation) 의 admin endpoints 가 현재 unauthed. 외부 노출 시점에 TLS + 추가 인증 필요.

### 대안 검토

| 방식 | 장점 | 단점 |
|---|---|---|
| **Optional env-driven TLS (이번 ADR)** | 기본 OFF (모든 demo 호환), opt-in | TLS off 가 default — 운영자 명시 필요 |
| 항상 TLS | 보안 default | 모든 demo + script 변경 필요, self-signed cert handling 복잡 |
| Reverse proxy (nginx/envoy) 위임 | edge 코드 변경 0 | 추가 컴포넌트, 설정 분산 |
| Cilium/SPIFFE service mesh | 표준 zero-trust | k8s 환경 전제, 데모와 어울리지 않음 |

ADR-013/027/028 의 opt-in 패턴 일관성 + demo 호환을 위해 **env-driven optional TLS** 채택. 운영자가 cert 준비 후 env 설정.

### 설계 범위 (명시)

이번 ADR 범위:
- **Edge HTTPS** — `EDGE_TLS_CERT` + `EDGE_TLS_KEY` 환경변수 설정 시 `ListenAndServeTLS`
- **Edge → DN HTTPS client** — `EDGE_DN_TLS_CA` (DN 인증서 신뢰 CA) 설정 시 https:// 사용
- **DN HTTPS server** — `DN_TLS_CERT` + `DN_TLS_KEY` 설정 시 TLS 서버
- **mTLS 옵션** — edge 가 client cert 제시 (`EDGE_DN_TLS_CLIENT_CERT/KEY`), DN 이 검증 (`DN_TLS_CLIENT_CA`)
- **Self-signed cert 생성 스크립트** (`scripts/gen-tls-certs.sh`) — 데모용

이번 ADR 비범위:
- Cert 자동 갱신 (Let's Encrypt 등)
- ALPN / HTTP/2 명시 설정 (Go stdlib 기본 활용)
- Client → edge mTLS (UrlKey HMAC 으로 충분; SDK 클라이언트 인증 별도)
- Cert revocation list / OCSP

## 결정

### env vars (전부 optional)

```
# Edge 외부 (HTTPS server)
EDGE_TLS_CERT      cert 파일 경로 (PEM)
EDGE_TLS_KEY       key 파일 경로 (PEM)
   ↑ 둘 다 설정 → ListenAndServeTLS, 둘 다 미설정 → 평문 HTTP

# Edge → DN (HTTPS client)
EDGE_DN_TLS_CA     DN cert 신뢰 CA (PEM). 설정 시 edge 가 https:// 로 DN 접근
EDGE_DN_TLS_CLIENT_CERT / EDGE_DN_TLS_CLIENT_KEY
                   mTLS: edge 가 DN 에 제시할 client cert

# DN (HTTPS server)
DN_TLS_CERT / DN_TLS_KEY      DN HTTPS server
DN_TLS_CLIENT_CA              mTLS: edge 의 client cert 검증할 CA
```

### URL scheme 결정

edge 가 DN URL 을 build 할 때 (`http://addr/chunk/...`):
- `EDGE_DN_TLS_CA` 설정 → `https://`
- 미설정 → `http://`

DN address 자체 (`dn1:8080`) 는 변경 없음 — scheme 은 client 가 결정.

### Coordinator 변경

```go
type Config struct {
    ...existing...
    DNScheme string  // "http" or "https" (default "http")
    TLSConfig *tls.Config  // nil = no TLS for client
}

func New(cfg Config) (*Coordinator, error) {
    ...
    httpClient := &http.Client{Timeout: cfg.Timeout}
    if cfg.TLSConfig != nil {
        httpClient.Transport = &http.Transport{TLSClientConfig: cfg.TLSConfig}
    }
    ...
}
```

URL build 시 `c.scheme + "://" + addr + "/chunk/" + id`.

### 스크립트 (`scripts/gen-tls-certs.sh`)

자체 서명 CA 1개 + edge cert + DN cert + edge client cert (mTLS) 를 한 번에 생성. openssl 사용. 데모용 단일 CA 모두 신뢰.

생성 결과:
```
certs/
  ca.crt           # 모든 컴포넌트의 신뢰 root
  edge.crt + edge.key   # edge HTTPS server
  dn.crt + dn.key       # DN HTTPS server (모든 DN 공유 — SAN 에 dn1..dn9)
  edge-client.crt + edge-client.key   # edge → DN mTLS client
```

데모 목적 단순화: cert 1개를 모든 DN 이 공유 (SAN=`dn1,dn2,...,dn9`). 실 운영은 DN 마다 개별 cert 권장.

### `scripts/up-tls.sh` (옵션 데모)

`up.sh` 의 TLS 버전. `gen-tls-certs.sh` 후 모든 컨테이너에 cert 마운트 + env 설정.

### 명시적 비범위

- **client → edge mTLS** — UrlKey HMAC 가 application-layer 인증 담당. mTLS 추가 시 ADR 별도
- **cert 갱신 자동화** — 운영자가 cert 갱신 후 edge restart (또는 SIGHUP 핸들러는 별도 ADR)
- **인증 + admin endpoint** — 현재 admin endpoints 가 unauthed. TLS 켜진 상태에서도 unauthed → public 노출 시 추가 보호 필요. 후속 ADR

## 결과

### 긍정

- **opt-in default OFF** — 11 기존 demos 영향 0
- **Go stdlib 활용** — `crypto/tls` + `net/http` 기본 동작
- **mTLS 옵션** — production 등급 zero-trust 가능
- **데모 cert 생성 단순화** — `gen-tls-certs.sh` 한 번
- **env 만으로 활성** — 코드 수정 없음

### 부정

- **cert 관리 부담 운영자** — 갱신/배포는 별도
- **default OFF** — 운영자가 명시적 활성 안 하면 평문 그대로
- **self-signed cert 의 보안 환상** — 데모용. 실 인증서 (Let's Encrypt 등) 권장

### 트레이드오프 인정

- "왜 default ON 안 함?" — 11 기존 데모 + script 모두 영향. opt-in 으로 backward compat
- "왜 service mesh 안 씀?" — k8s 의존, 데모 환경 부적합
- "왜 cert 자동 갱신 안 함?" — operator concern, 별도 ADR

## 데모 시나리오

```
1. ./scripts/gen-tls-certs.sh                # certs/ 디렉토리 생성
2. ./scripts/up-tls.sh                       # cert 마운트 + env 설정으로 cluster 기동
3. curl --cacert certs/ca.crt https://localhost:8443/healthz   # ✅ TLS PUT/GET
4. (없는 cert 로 시도) curl https://localhost:8443/healthz     # ❌ cert verify fail
```

기존 demos:
- `up.sh` (TLS 안 켬) → http:// 그대로
- TLS 데모 사용 시 `up-tls.sh` 별도

## 관련

- ADR-007 (UrlKey HMAC) — application 인증, TLS 와 직교
- ADR-027/028 (admin endpoints) — TLS off 상태에서 unauthed; 추가 보호 후속
- `internal/coordinator/` — DNScheme + TLSConfig 지원
- `cmd/kvfs-edge/` `cmd/kvfs-dn/` — TLS env 파싱
- `scripts/gen-tls-certs.sh` — self-signed cert 생성
- `scripts/up-tls.sh` — TLS 데모 cluster

## 후속 ADR 예상

- **ADR-033 — Admin endpoint authentication**: TLS + mTLS 또는 token-based
- **ADR-034 — Cert auto-renewal**: SIGHUP / Let's Encrypt integration
- **ADR-035 — Per-DN unique cert**: SPIFFE-style identity
