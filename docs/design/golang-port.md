# memory-mcp Go 포팅 테크스펙

| 항목 | 내용 |
|---|---|
| 목표 | TypeScript 구현(src/ 11개 모듈, 테스트 29개)을 동작 동일성(behavior parity) 기준으로 Go로 전환 |
| 작성일 | 2026-08-25 |
| 전제 | [architecture.md](architecture.md)가 설계의 정본. 이 문서는 **언어 매핑 결정만** 다룬다 — 설계 변경 없음 |
| 상태 | **superseded** — [architecture-v2.md](architecture-v2.md)로 대체됨 (2026-08-25, SQLite 파리티 포팅 대신 OpenSearch+Neo4j+S3 재설계) |

## 1. 왜 Go인가

- **배포**: 현재는 Node ≥ 22.5 강제(node:sqlite 의존). Go는 단일 정적 바이너리 — `claude mcp add memory-mcp -- /path/to/memory-mcp` 한 줄로 끝나고 런타임 요구가 없다.
- **의존성 철학의 연장**: TS판의 "런타임 의존 2개(sdk, zod)" 철학을 Go에서도 유지 — sdk + SQLite 드라이버 2개로 억제.
- MCP 서버는 I/O 바운드 + 임베디드 SQLite라 성능 동기는 부차적. 주 동기는 배포·설치 단순화다.

## 2. 스택 결정

| 관심사 | TS판 | Go판 | 근거 |
|---|---|---|---|
| MCP SDK | `@modelcontextprotocol/sdk` + zod | `github.com/modelcontextprotocol/go-sdk` (공식) | 공식 SDK. 도구 입력 스키마는 struct 태그 기반 jsonschema 자동 생성 — zod의 역할을 대체 |
| SQLite | `node:sqlite` (SQLite 3.50) | `modernc.org/sqlite` (pure Go, CGO-free) | CGO 없이 크로스컴파일 가능 = 단일 바이너리 유지. FTS5 포함 빌드 |
| ULID | 자체 구현 54줄 | 자체 포팅 (`crypto/rand` + Crockford base32) | 의존성 2개 상한 유지. 테스트 이미 존재 |
| frontmatter | 자체 엄격 파서 209줄 | 자체 포팅 | "엄격한 YAML 부분집합" 보장은 라이브러리가 아니라 파서 코드 자체가 스펙. yaml lib 도입 금지 |
| 해시 | node:crypto sha256 | `crypto/sha256` stdlib | — |
| 스키마 검증 | zod | go-sdk jsonschema + 핸들러 진입부 수동 검증(enum 등) | zod의 refine급 검증은 핸들러에서 명시적으로 |
| 린트/포맷 | biome | `gofmt` + `golangci-lint` | — |
| 테스트 | `node --test` | `go test` (table-driven) | — |

### 2.1 리스크: modernc.org/sqlite의 FTS5 trigram

검색 계획기 전체(§architecture 8)가 `fts_word`(unicode61) + `fts_tri`(trigram) + `bm25()`에 의존한다. trigram 토크나이저는 SQLite 3.34+ FTS5 코어에 포함되므로 modernc 빌드에 있어야 하지만, **Phase 0에서 스파이크로 실측 확인**한다 (`CREATE VIRTUAL TABLE ... tokenize='trigram'` + `remove_diacritics 2` + `bm25()` + `ESCAPE` LIKE).

- 실패 시 폴백: `mattn/go-sqlite3` + `fts5` 빌드 태그. CGO가 생겨 크로스컴파일이 무거워지는 비용 — 폴백은 최후 수단.

### 2.2 동시성 (TS에 없던 관심사)

Node는 단일 스레드라 store 접근이 자연 직렬화됐다. go-sdk는 도구 핸들러를 고루틴에서 실행할 수 있으므로 **store 단위 `sync.Mutex`로 도구 호출 전체(reconcile 포함)를 직렬화**한다. 설계가 call-time reconcile 전제라 직렬화가 정합성의 최소 비용이고, 로컬 단일 에이전트 워크로드에서 병목이 아니다.

## 3. 모듈 매핑

모듈 경계는 1:1로 유지한다 (검증된 구조 — 재설계 없음).

| TS | Go | 비고 |
|---|---|---|
| `src/types.ts` | `internal/memory/types.go` | 상수(가중치·상태·타입)와 도메인 타입. string literal union → 타입드 상수 + 검증 함수 |
| `src/ulid.ts` | `internal/ulid/` | |
| `src/frontmatter.ts` | `internal/frontmatter/` | |
| `src/lifecycle.ts` | `internal/memory/lifecycle.go` | 상태 전이 테이블 |
| `src/dictionary.ts` | `internal/dictionary/` | |
| `src/db.ts` | `internal/index/` | DerivedIndex. `database/sql` + modernc 드라이버 |
| `src/store.ts` | `internal/store/` | reconcile·경로 컨테인먼트·파일 I/O |
| `src/search.ts` | `internal/search/` | 질의 계획기 + 랭킹. CJK 판정은 `unicode` rangetable |
| `src/conflict.ts` | `internal/conflict/` | pair-hash·이웃 감지 |
| `src/maintenance.ts` | `internal/maintenance/` | status·compaction 후보 |
| `src/server.ts` | `internal/server/` | 도구 7개 등록. ok/fail 봉투 동일 JSON |
| `src/main.ts` | `cmd/memory-mcp/main.go` | stdio transport |

모듈 경로: `github.com/drakejin/memory-mcp`.

### 3.1 포팅 시 언어차 주의점

- **JSON 응답 형태 고정**: 도구 응답의 필드명·생략 규칙(TS의 `|| undefined` → Go `omitempty`)을 TS판과 바이트 수준이 아닌 **구조 수준으로 동일**하게. 파리티 테스트의 비교 대상.
- **정렬 안정성**: JS `Array.sort` → Go `sort.SliceStable`. 랭킹 동점 시 순서 차이가 파리티 테스트를 흔들지 않게 tie-breaker(id) 명시.
- **시간**: TS는 ISO 문자열을 그대로 저장·비교. Go도 `time.Time` 변환 없이 **문자열 그대로** 다뤄 사전순 비교 유지(현재 설계가 그렇다). `mtime_ns`는 `ModTime().UnixNano()`를 문자열로.
- **정규식**: 계획기의 토큰 정제·CJK 판정 정규식을 RE2로 이식 — lookbehind 등 RE2 불가 패턴 없음(확인 완료).

## 4. 작업 순서

TDD: 각 모듈은 기존 TS 테스트를 Go table-driven으로 먼저 포팅(RED) 후 구현(GREEN).

| Phase | 내용 | 완료 기준 |
|---|---|---|
| 0. 스파이크 | modernc.org/sqlite에서 §2.1 기능 실측 | trigram·bm25·unicode61 옵션 전부 동작 확인 스크립트 |
| 1. 기반 | go.mod, 브랜치 `golang-port`, leaf 모듈(ulid·frontmatter·lifecycle·dictionary·types) | 해당 TS 테스트 전부 Go로 통과 |
| 2. 저장 | index(db) + store(reconcile) | store.test.ts 포팅 통과 |
| 3. 검색·충돌 | search + conflict + maintenance | search/conflict 테스트 포팅 통과 |
| 4. 표면 | server(도구 7개) + main | 수동 e2e: Claude Code에 등록해 7개 도구 왕복 |
| 5. 파리티 | 동일 fixture store에 TS/Go 양쪽 실행, 도구 응답 diff | recall 랭킹·status 집계·충돌 감지 결과 일치 |
| 6. 전환 | TS 소스·pnpm 제거, README 갱신, CI(go test + goreleaser) | `go install`/릴리스 바이너리로 설치 동작 |

Phase 5까지 TS는 레포에 남긴다 — 파리티 기준선. 제거는 6에서 한 번에.

## 5. 테스트 계획

- 기존 29개 테스트 전량 포팅이 하한선. 커버리지 80% 이상 (`go test -cover`).
- 추가: 동시 도구 호출 레이스 테스트(`-race`), Phase 5 파리티 fixture(한국어 조사 변형 검색 케이스 포함).
- e2e: MCP stdio 왕복은 go-sdk의 in-memory transport로 서버 수준 테스트 1벌.

## 6. 배포

- `goreleaser`: darwin/linux × amd64/arm64. CGO-free라 매트릭스 빌드 단순.
- README 설치 절차: 릴리스 바이너리 다운로드 또는 `go install github.com/drakejin/memory-mcp/cmd/memory-mcp@latest`.

## 7. 비목표

- 설계 변경(도구 표면·랭킹 공식·상태기계) — architecture.md 그대로
- 성능 최적화(벤치마크는 파리티 이후 관심사)
- 임베딩·형태소 등 architecture.md §11의 기존 비목표 전부 유지

## 8. 열린 결정 (리뷰 요청)

1. **저장소 전략**: 같은 레포 in-place 전환(제안) vs `memory-mcp-go` 별도 레포
2. **TS 제거 시점**: Phase 6 일괄 제거(제안) vs 당분간 공존
3. **모듈 경로**: `github.com/drakejin/memory-mcp` 가정 — 배포 계정 확인 필요
