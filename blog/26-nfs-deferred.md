# Episode 26 — NFS gateway: 왜 안 만들었는가

> **Series**: kvfs — distributed object storage from scratch
> **Season**: P4-09 wave · **Episode**: 13
> **연결**: [ADR-032 NFS gateway deferred](../docs/adr/ADR-032-nfs-gateway-deferred.md)

---

## "그거 NFS 로 mount 되나요?"

S4 close 후 받은 질문. POSIX-like 인터페이스가 있으면 기존 도구 (rsync, tar, ls) 가 그대로 사용 가능. MinIO 도 GoFuse, JuiceFS 도 NFS gateway 가 인기 기능.

평가 후 결론: **하지 마라**. ADR-032 가 거절 사유 기록. 이 글은 그 회고.

## kvfs 의 데이터 모델

- bucket / key — flat namespace + key 안의 `/` 가 prefix tree
- chunk content-addressable (sha256)
- 객체 = chunks 집합 (replication) 또는 stripes 집합 (EC)
- 원자성 단위: 객체 전체 (PUT/GET/DELETE)

POSIX 가 요구하는 것:
- inode (= 파일 ID), inode 안에 byte 배열, 부분 read/write
- directory entry (parent inode + name → child inode)
- mtime, ctime, atime (특히 atime — read 시 update)
- permission bits, uid/gid
- hard link, symlink
- rename (atomic)
- append, partial overwrite, truncate

거의 0% 일치.

## 가능한 두 길

### 길 1: FUSE 마운트 (사용자 공간)

`bazil/fuse` 또는 `hanwen/go-fuse` 위에 daemon. POSIX 호출을 가로채 kvfs HTTP API 로 변환.

문제:
- **Partial write**: `pwrite(fd, buf, 1KB, 100MB-1KB)` 가 들어오면? kvfs 는 chunk 단위 immutable. 100 MB 객체 다시 PUT 하든가, 아니면 chunk 분할 + 일부 chunk 만 새로 쓰든가. 후자는 chunker 모델 깨짐.
- **append**: open(O_APPEND) → write(1 byte) ×N. 매 byte 가 객체 mutate. CDC 의 dedup 효과 가깝게 살리려면 buffer + flush 정책 필요. 결과 = local "write-back cache" 라는 새 컴포넌트.
- **rename**: `rename(a, b)` 는 POSIX 에서 atomic. kvfs 는 PUT new + DELETE old — race. metadata 트랜잭션 필요.
- **stat 폭격**: `ls -la` 가 directory 의 모든 entry 에 stat — 100 entry 디렉토리 = 100 lookup. 대규모에서 latency 폭발.

이 모두 풀려면 daemon 안에 사실상 작은 filesystem cache. kvfs 본체가 아니라 그 fuse-shim 이 더 복잡.

### 길 2: NFS server 직접

`willscott/go-nfs` 같은 stdlib-only NFS v3 implementation. 같은 trade-off + 추가로 NFS 의 stateless protocol idiom 맞춰주기.

같은 결론.

## 더 깊은 문제: stateful filesystem 으로 변질

POSIX 호환을 하나하나 채우면 kvfs 는 더 이상 object storage 가 아니다. fs 가 된다.

- atime update 마다 메타 mutation? 그러면 read 가 write — bbolt single-writer 폭발.
- inode 의 byte 모델 → block-level allocator 필요.
- directory operations 의 atomicity → 전역 lock 또는 트랜잭션 manager.

kvfs 의 매력 (단순함, content-addressable, immutable chunk) 가 모두 깨진다. **다른 시스템을 만드는 셈**.

## 사용자가 진짜 원하는 것

대부분의 "NFS mount 되나요?" 는 사실 다음 중 하나:

| 진짜 needs | kvfs 의 답 |
|---|---|
| "rsync 로 백업하고 싶다" | `rclone copy local/ kvfs:bucket/` (S3 backend 호환 — 아래) |
| "Jenkins artifact 저장" | S3 SDK 그대로 (S3-compatible API, P4-09) |
| "Spark/Trino 데이터 lake" | S3 SDK |
| "ML training dataset mount" | s3fs-fuse 같은 third-party shim — 가벼움, 운영자 책임 |
| "임시 파일 공유, 누가 ls 해야 함" | "이건 NFS 가 정답, kvfs 가 아님" |

마지막 case 는 kvfs 가 풀어야 할 문제가 아니다 — `nfs-ganesha` 또는 OS의 NFS server 가 정답. kvfs 가 그 자리를 차지하려 하면 둘 다 못 한다.

## S3-compatible API 로 대체

P4-09 wave 가 답한 것: **HEAD + ListBucket** 추가 (full S3 API 의 작은 부분 집합). rclone, aws-cli, s3fs-fuse 가 모두 동작. POSIX-shim 이 정 필요하면 사용자가 s3fs-fuse 를 자기 책임으로 띄우면 된다.

```
$ s3cmd --host=edge:8000 --no-ssl ls s3://bucket/
                          PRE    prefix1/
2026-04-26 13:00     12345  s3://bucket/file.txt

$ aws s3 cp ./local.dat s3://bucket/local.dat \
    --endpoint-url=http://edge:8000
```

S3 surface 가 완벽하지 않지만 90% use case 처리. 나머지 10% 는 객체 storage 로 풀 수 없는 본질적 fs 작업.

## 미래 가능성

ADR-032 은 영구 거절이 아니라 "지금은 미루기" 의 명시적 기록. 채택되려면:
- POSIX semantic 의 어느 subset 이 충분한지 명시 (예: read-only mount + listdir 만)
- 그 subset 의 복잡도가 위 path 1/2 보다 작은지 측정
- 제3자가 관심 표명 (PR or issue)

사용자가 이 글 읽고 "내 use case 는 read-only listdir 면 충분" 같은 input 줘야 다시 평가됨.

## 교훈

**거절도 결정**. 안 한 일을 ADR 로 박으면 6개월 후 같은 토론 반복 안 함. "미래의 나" 가 같은 질문 받으면 ADR 링크 한 줄 답.

## 다음

[Ep.27 WAL log compaction](27-log-compaction.md): WAL 이 영원히 자라지 않게.
