// Package rebalance migrates existing chunks to their currently-desired DNs.
//
// ──────────────────────────────────────────────────────────────────
// 비전공자용 해설
// ──────────────────────────────────────────────────────────────────
//
// 풀어야 할 문제 (ADR-010 의 동기)
// ────────────────────────────────
// ADR-009 (Rendezvous Hashing) 으로 **새 쓰기** 는 항상 현재 토폴로지에 맞춰
// 상위 R 개 DN 에 간다. 그러나 **DN 추가 직후의 기존 청크** 는 쓰기 시점에
// 결정된 DN 에 그대로 누워 있다. 새 DN(예: dn4) 디스크에는 그 청크가 0개.
//
//   meta.Replicas      = ["dn1:8080", "dn2:8080", "dn3:8080"]   ← 쓰기 시점 사실
//   placer.Pick(...)   = ["dn1:8080", "dn4:8080", "dn3:8080"]   ← 지금이라면
//                                       ↑ dn4 는 청크 없음
//
// 이 패키지는 두 집합을 비교해 **누락된 DN(missing)** 으로 청크를 복사하고
// 메타의 Replicas 배열을 갱신한다.
//
// 안전 규칙: copy-then-update, never delete
// ─────────────────────────────────────────
// "새 자리에 복사 → 메타 갱신 → 이전 자리에서 삭제" 의 마지막 단계를 **하지 않는다**.
// 즉 over-replicate 를 허용한다. 이유:
//   - 마이그레이션 도중 서버가 죽어도 청크가 살아있음
//   - 멱등성 — 같은 plan 을 두 번 돌려도 안전 (같은 chunk_id 면 PUT 덮어쓰기)
//   - 디스크 사용량 일시 증가는 받아들이는 트레이드오프
//
// 잉여 청크(surplus) 정리는 **별도 GC 패스** (ADR-012 예상) 의 책임이다.
//
// 동시성·실패 처리
// ───────────────
//   - Run 은 concurrency goroutine 으로 청크 단위 병렬 처리
//   - 한 청크의 부분 실패: 성공한 dst 만 메타에 추가 (다음 run 이 나머지 재시도)
//   - 소스 GET 자체 실패 (모든 actual DN 죽음): 그 청크는 손상 위험. 별도 alert
//     필요 — 현 ADR 범위 밖. 통계의 Failed 카운터에만 기록
//
// 인터페이스 분리
// ──────────────
// Coordinator·ObjectStore 를 인터페이스로 받는다. 이유:
//   - 테스트에서 fake 주입 가능 (네트워크·디스크 없이 알고리즘 검증)
//   - 미래에 별도 daemon 으로 분리해도 같은 패키지 재사용
package rebalance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/HardcoreMonk/kvfs/internal/store"
)

// Coordinator is the subset of *coordinator.Coordinator used by rebalance.
// Defined as an interface for testability.
type Coordinator interface {
	// PlaceChunk returns the addresses currently desired for the given chunk
	// according to the live placement (Rendezvous Hashing).
	PlaceChunk(chunkID string) []string

	// ReadChunk fetches the chunk body from the first responsive candidate.
	// Returns body, address-served-from, error.
	ReadChunk(ctx context.Context, chunkID string, candidates []string) ([]byte, string, error)

	// PutChunkTo writes a chunk to a single DN address (no fanout, no quorum).
	PutChunkTo(ctx context.Context, addr, chunkID string, data []byte) error
}

// ObjectStore is the subset of *store.MetaStore used by rebalance.
type ObjectStore interface {
	ListObjects() ([]*store.ObjectMeta, error)
	UpdateReplicas(bucket, key string, replicas []string) error
}

// Migration is a single chunk's planned move.
//
// Actual / Desired / Missing / Surplus 는 모두 정렬된 슬라이스로 결정성 보장.
type Migration struct {
	Bucket  string   `json:"bucket"`
	Key     string   `json:"key"`
	ChunkID string   `json:"chunk_id"`
	Size    int64    `json:"size"`
	Actual  []string `json:"actual"`            // 메타에 적힌 현재 replica 주소
	Desired []string `json:"desired"`           // placement.Pick 의 현재 결과
	Missing []string `json:"missing"`           // desired - actual : 복사 대상
	Surplus []string `json:"surplus,omitempty"` // actual - desired : 정보용 (삭제 안 함)
}

// Plan summarizes what would change. Pure read — no side effect.
type Plan struct {
	Scanned    int         `json:"scanned"`             // 메타에서 본 객체 수
	Migrations []Migration `json:"migrations"`          // Missing 이 빈 것은 제외
	GeneratedAt string     `json:"generated_at,omitempty"`
}

// RunStats reports the outcome of executing a Plan.
type RunStats struct {
	Migrated    int      `json:"migrated"`     // 메타까지 갱신 완료된 청크 수
	Failed      int      `json:"failed"`       // 부분 실패 또는 전체 실패
	BytesCopied int64    `json:"bytes_copied"` // 누적 복사 바이트 (size × dst 수)
	Errors      []string `json:"errors,omitempty"`
}

// ComputePlan walks all objects, comparing desired vs actual placement.
// Read-only: never modifies coord, store, or any DN.
func ComputePlan(coord Coordinator, st ObjectStore) (Plan, error) {
	objs, err := st.ListObjects()
	if err != nil {
		return Plan{}, fmt.Errorf("rebalance: list objects: %w", err)
	}
	plan := Plan{Scanned: len(objs)}
	for _, obj := range objs {
		desired := sortedCopy(coord.PlaceChunk(obj.ChunkID))
		actual := sortedCopy(obj.Replicas)
		missing := setDiff(desired, actual)
		surplus := setDiff(actual, desired)
		if len(missing) == 0 {
			continue
		}
		plan.Migrations = append(plan.Migrations, Migration{
			Bucket:  obj.Bucket,
			Key:     obj.Key,
			ChunkID: obj.ChunkID,
			Size:    obj.Size,
			Actual:  actual,
			Desired: desired,
			Missing: missing,
			Surplus: surplus,
		})
	}
	return plan, nil
}

// Run executes the plan with the given concurrency.
// concurrency <= 0 falls back to 1.
//
// 한 청크 처리 순서:
//  1. Actual DN 중 한 곳에서 청크 GET (Coordinator.ReadChunk 가 첫 성공 반환)
//  2. Missing 의 각 DN 으로 PutChunkTo
//  3. 성공한 dst 만 unique(actual ∪ successful) 로 메타 갱신
//
// 모든 Missing 이 성공해야 Migrated++, 그렇지 않으면 Failed++.
// 부분 성공도 메타는 갱신함 — 다음 run 이 나머지 시도 가능.
func Run(ctx context.Context, coord Coordinator, st ObjectStore, plan Plan, concurrency int) RunStats {
	if concurrency <= 0 {
		concurrency = 1
	}
	stats := RunStats{}
	if len(plan.Migrations) == 0 {
		return stats
	}

	type result struct {
		ok          bool
		bytesCopied int64
		err         string
	}
	resultCh := make(chan result, len(plan.Migrations))

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, mg := range plan.Migrations {
		wg.Add(1)
		mg := mg // capture
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			resultCh <- migrateOne(ctx, coord, st, mg)
		}()
	}
	go func() { wg.Wait(); close(resultCh) }()

	for r := range resultCh {
		if r.ok {
			stats.Migrated++
		} else {
			stats.Failed++
		}
		stats.BytesCopied += r.bytesCopied
		if r.err != "" {
			stats.Errors = append(stats.Errors, r.err)
		}
	}
	return stats
}

func migrateOne(ctx context.Context, coord Coordinator, st ObjectStore, mg Migration) (out struct {
	ok          bool
	bytesCopied int64
	err         string
}) {
	if len(mg.Actual) == 0 {
		out.err = fmt.Sprintf("%s/%s: no actual replicas to read from", mg.Bucket, mg.Key)
		return
	}
	data, _, err := coord.ReadChunk(ctx, mg.ChunkID, mg.Actual)
	if err != nil {
		out.err = fmt.Sprintf("%s/%s: read source: %v", mg.Bucket, mg.Key, err)
		return
	}

	successful := append([]string(nil), mg.Actual...)
	var copyErrs []string
	for _, dst := range mg.Missing {
		if err := coord.PutChunkTo(ctx, dst, mg.ChunkID, data); err != nil {
			copyErrs = append(copyErrs, fmt.Sprintf("%s: %v", dst, err))
			continue
		}
		successful = append(successful, dst)
		out.bytesCopied += int64(len(data))
	}

	finalReplicas := uniqueSorted(successful)
	if updateErr := st.UpdateReplicas(mg.Bucket, mg.Key, finalReplicas); updateErr != nil {
		out.err = fmt.Sprintf("%s/%s: update meta: %v", mg.Bucket, mg.Key, updateErr)
		return
	}

	if len(copyErrs) == 0 {
		out.ok = true
		return
	}
	out.err = fmt.Sprintf("%s/%s: partial copy: %s", mg.Bucket, mg.Key, joinErrs(copyErrs))
	return
}

// ─── helpers ───

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// setDiff returns elements in a but not in b. Both must be sorted (or sorted before pass).
func setDiff(a, b []string) []string {
	bset := make(map[string]struct{}, len(b))
	for _, x := range b {
		bset[x] = struct{}{}
	}
	var out []string
	for _, x := range a {
		if _, ok := bset[x]; !ok {
			out = append(out, x)
		}
	}
	return out
}

func uniqueSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, x := range in {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}

func joinErrs(errs []string) string {
	out := ""
	for i, e := range errs {
		if i > 0 {
			out += "; "
		}
		out += e
	}
	return out
}

// ErrEmptyPlan signals Run was called with no migrations. Reported for diagnostics
// — callers may treat as success.
var ErrEmptyPlan = errors.New("rebalance: plan has no migrations")
