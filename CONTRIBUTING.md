# Contributing to kvfs

kvfs는 분산 object storage 의 핵심 원리를 시연하는 **교육적 레퍼런스** 입니다. 프로덕션 시스템이 아닙니다 — 코드의 우선순위는 **명료성·검증가능성·완결성** 입니다.

## 기여 환영하는 것

- **버그 수정**: 데모/테스트가 실제로 깨지는 경우. 재현 단계 명시
- **문서/주석 개선**: ADR·블로그·코드 주석 (특히 비전공자 친화 설명)
- **테스트 추가**: 기존 코드의 누락된 boundary case
- **새 ADR**: `docs/FOLLOWUP.md` 의 미해결 후보(P8-05/P8-07/P8-17, P6-08/P6-12 등) 또는 issue 에서 합의된 새 후보. PR 보다 먼저 논의 권장

## 신중한 영역 (PR 전에 issue 로 논의 권장)

- **새 외부 의존성** — 현재 `bbolt` 1개. 추가 시 ADR 필요
- **public API 변경** — `internal/edge` HTTP 엔드포인트, CLI 명령 추가/변경
- **새 ADR** — 본문 + supersede 관계 + 기존 ADR 와 일관성 검토

## 환영하지 않는 것 (현 단계)

- **성능 최적화 PR** — 가독성을 해치는 SIMD/inline assembly. 별도 ADR (ADR-017/018/019 등) 트랙으로
- **새 기능 (feature creep)** — kvfs 의 목표는 production-ready 가 아님. 새 기능은 ADR 부터
- **광범위한 리포맷팅/스타일 변경** — 합의된 컨벤션 외 변경

## 개발 흐름

### 빌드 + 테스트

```bash
# 외부 도구 없이 (도커만)
docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=0 \
  golang:1.26-alpine sh -c 'go build ./... && go test ./...'

# 로컬에 Go 1.26+ 가 있으면
make build && make test && make lint
```

### 라이브 데모

```bash
docker build -t kvfs-edge:dev --target kvfs-edge .
docker build -t kvfs-dn:dev   --target kvfs-dn   .
./scripts/demo-alpha.sh    # 3-way replication
./scripts/demo-iota.sh     # chunking (Season 2)
./scripts/demo-kappa.sh    # Reed-Solomon EC (Season 2)
./scripts/demo-lambda.sh   # auto-trigger (Season 3)
./scripts/demo-rho.sh      # multi-edge HA read-replica (Season 3)
./scripts/down.sh          # cleanup
# 전체 데모 매핑은 README.md § "설계 결정" 표 참조
```

### 문서 드리프트 검사

```bash
./scripts/check-doc-drift.sh
git diff --check
```

새 env, ADR, demo, blog episode 를 추가하면 `README.md`, `docs/GUIDE.md`,
`docs/guide.html`, `docs/adr/README.md`, `docs/FOLLOWUP.md` 를 함께 확인합니다.
에이전트 작업 규약은 `AGENTS.md` 가 단일 소스이고 `CLAUDE.md` 는 호환 shim 입니다.

### 커밋 메시지

`<type>(<scope>): <subject>` 형식 권장:
- `feat(rebalance): ...`, `fix(gc): ...`, `docs(adr-013): ...`, `refactor(auto): ...`
- 본문에 **why** 명시 (what 은 diff 가 보여줌)

### Pull Request

1. fork → branch → PR
2. CI (`go build`, `go vet`, `go test`, `staticcheck`, `govulncheck`) 통과 확인
3. 새 코드는 테스트 추가 (특히 boundary)
4. ADR 도입 시: PR 본문에 ADR 링크 + supersede 관계 명시
5. docs 변경 시: `./scripts/check-doc-drift.sh` 결과 확인
6. blog episode 도입 시: 라이브 데모 출력을 markdown 에 embed (스크린샷 X)

## ADR 작성 규칙

- 한국어 + 한 페이지 이내
- `docs/adr/ADR-NNN-short-slug.md` (3자리 0-padded)
- 형식: 상태 → 맥락 → 대안 검토 → 결정 → 결과 (긍정/부정/트레이드오프) → 데모 시나리오 → 관련 → 후속 ADR
- "하나의 결정 = 하나의 보장" 원칙 — 명시적 비범위 섹션 필수

## 라이선스

기여하는 모든 코드는 [Apache License 2.0](LICENSE) 하에 배포됩니다. 모든 `.go` 파일 상단에 SPDX 헤더가 있어야 합니다 (`make license` 으로 자동).

## Code of Conduct

[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) — Contributor Covenant 2.1.

## 질문

- GitHub Issues 우선 (한국어 또는 영어)
- 답변 보장 안 함 (개인 프로젝트)
