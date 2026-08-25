# 07 — 재수화와 degraded 모드: 컨테이너는 언제든 죽는다

파생 저장소(OpenSearch·Neo4j)를 볼륨 없이 띄우고, manifest 대조로 드리프트를 판정해 hot JSON에서 다시 세우며, 파생물이 죽은 동안에도 쓰기를 성공시키는 규칙을 정의한다.

| 항목 | 내용 |
|---|---|
| 관련 코드 | `internal/rehydrate/rehydrate.go` · `internal/rehydrate/hydrate.go` · `internal/rehydrate/manifest.go` · `internal/server/startup.go` · `internal/server/degraded.go` · `internal/server/handlers_ops.go` · `deploy/docker-compose.yml` |
| 관련 스펙 | [02-storage-model](02-storage-model.md)(manifest·hot 정본) · [04-episodic-search](04-episodic-search.md)(bulk 색인) · [05-knowledge-graph](05-knowledge-graph.md)(MERGE 멱등성) · [06-documents](06-documents.md)(문서 ingest 저하) · [08-http-api](08-http-api.md)(봉투·상태코드) · [10-operations](10-operations.md) · [11-testing](11-testing.md) · 인덱스: [README](README.md) |
| 상태 | 구현 완료. 단위 테스트 `internal/rehydrate/*_test.go` (`go test ./internal/rehydrate/... -cover` = 94.4%), 블랙박스 `TestScenario05_Rehydration` / `TestScenario07_DegradedMode` / `TestScenario08_StatusHonesty` |

![재수화와 degraded 모드 타임라인](assets/07-rehydration.svg)

---

## 1. 볼륨을 붙이지 않는 결정

`deploy/docker-compose.yml`에는 두 서비스 모두 `volumes:` 항목이 없다. 파일 첫 줄 주석이 그 이유를 못 박는다.

```
# Derived stores only (architecture-v2.md §8). Both containers are intentionally
# NON-persistent: no volumes. The hot JSON store owns all content; rehydration
# rebuilds these from scratch at any time (§5).
```

| 구성요소 | 실행 위치 | 영속성 |
|---|---|---|
| memory-mcp 서버 | 호스트 프로세스 (`make run` / `make start`) | — |
| `dj-memory-opensearch` (`deploy/opensearch/` 커스텀 이미지, nori 플러그인) | Docker | 없음 |
| `dj-memory-neo4j` (`neo4j:5-community`) | Docker | 없음 |
| hot 정본 `~/.local/dj-memory/` | 호스트 파일시스템 | 있음 (유일한 정본) |

이 결정이 안전한 근거는 세 가지다.

1. **콘텐츠 소유권이 hot에 있다.** `internal/hotstore`가 모든 쓰기를 temp+rename 원자 쓰기(`writeFileAtomic`)로 처리하고, 패키지 주석은 "content that exists only in a derived store is a bug"라고 못 박는다.
2. **두 평면 모두 hot에서 전량 재구성 가능하다.** episodic은 인덱스를 통째로 버리고 `IndexRecords`로 다시 넣고, knowledge는 `Clear` 후 `MERGE` 멱등 upsert를 재생한다(§5).
3. **불일치를 사람의 감이 아니라 manifest로 판정한다.** 카운트·dirty·해시 세 가지 결정적 신호만 본다(§3).

서버 프로세스는 파생 저장소가 죽어 있어도 **부팅한다**. `cmd/memory-mcp/main.go`의 `build`는 `search.New` / `graph.New` / `cold.New` 실패를 `logger.Warn`으로만 남기고, 인터페이스 타입 변수(`search.Client` / `graph.Client` / `cold.Client`)를 nil로 둔 채 `server.Config`에 그대로 넘긴다. 생성자는 I/O를 하지 않으므로(code-standards §1 규칙 4), 실제 다운은 첫 `Ping`/요청에서 드러난다.

---

## 2. manifest — 대조의 유일한 근거

`~/.local/dj-memory/manifest.json` (`hotstore.Manifest`, `internal/hotstore/manifest.go`)

```go
type FileState struct {
    SHA256 string `json:"sha256"`
    // RecordCount is the derived-store unit count: episodic records for the
    // episodic plane, knowledge NODES (not edges) for the knowledge plane, so
    // rehydrate can compare it against OpenSearch doc-count / Neo4j node-count.
    RecordCount int       `json:"record_count"`
    IndexedAt   time.Time `json:"indexed_at"`
    // Dirty marks a best-effort derived upsert that failed; the next
    // rehydration pass converges it (§1).
    Dirty bool `json:"dirty"`
}

type IndexState struct {
    LastHydratedSHA string `json:"last_hydrated_sha"`
}

type Manifest struct {
    Files   map[string]FileState  `json:"files"`
    Indexes map[string]IndexState `json:"indexes"`
    // UpdatedAt is when the manifest itself was last rewritten.
    UpdatedAt time.Time `json:"updated_at"`
}
```

- 파일 키는 `hotstore.ManifestFileKey(plane, key)` 하나가 만든다 — `"{plane}/{ws}/{team}/{proj}"`. `internal/hotstore/key.go`가 이 형식의 단일 정본이고, `rehydrate`도 같은 함수를 호출한다(code-standards §4: 중복 헬퍼는 통합).
- 인덱스 키는 `internal/rehydrate/manifest.go`의 unexported `indexKeyEpisodic = "opensearch"` / `indexKeyKnowledge = "neo4j"`이며, 평면→키 변환의 **유일한 공개 진입점**이 `rehydrate.IndexKeyFor(plane)`다(`server.markIndexed`가 그것을 쓴다).
- `RecordCount`가 knowledge에서 **노드 수**인 이유는 `graph.NodeCount`와 직접 비교하기 위해서다(`FileState` 주석).
- 해시가 안정적인 이유: 모든 hot 파일은 `hotstore.marshalCanonical`(= `json.MarshalIndent(v, "", "  ")` + 개행 1개)로만 직렬화된다. 같은 내용은 항상 같은 sha256이 된다.

### 2.1 필드별 갱신 책임

| 누가 | 언제 | 무엇을 쓰는가 |
|---|---|---|
| `hotstore` `client.updateFileStateLocked` (`AppendEpisode`/knowledge 쓰기 경로) | hot 파일 바이트가 바뀔 때마다 | `SHA256`, `RecordCount`만. **`IndexedAt`·`Dirty`는 보존** — 파생 동기 상태는 다른 흐름의 소유물이다 |
| `server.markIndexed` (`internal/server/degraded.go`) | best-effort 파생 upsert **성공** 시 | `Dirty=false`, `IndexedAt=now`, `Indexes[IndexKeyFor(plane)].LastHydratedSHA = PlaneStateSHA(...)` |
| `server.markDirty` → `hotstore.Client.MarkDirty` | best-effort 파생 upsert **실패** 시 | `Dirty=true` (엔트리가 없으면 생성) |
| `rehydrate` `service.commitManifest` | 재수화 후 | 수렴한 프로젝트만 `touchFile`(Dirty 해제 + IndexedAt 갱신), **평면 전체가 수렴했을 때만** `Indexes[plane].LastHydratedSHA` 갱신 |

`markIndexed`/`markDirty`는 실패해도 `s.log.Error`만 남긴다. hot 쓰기가 이미 성공한 요청을 manifest 부기 실패로 뒤엎지 않는다.

### 2.2 `PlaneStateSHA` — 카운트가 못 잡는 것을 잡는다

```go
func PlaneStateSHA(m hotstore.Manifest, plane hotstore.Plane) string
```

한 평면(`"episodic/"` 또는 `"knowledge/"` 접두사)에 속한 manifest 파일 엔트리만 골라 키를 정렬한 뒤 `key + shaFieldSep + sha256 + shaEntrySep`(= `"\x00"`, `"\n"`)을 이어 sha256을 낸다. 두 구분자는 "다른 평면의 digest를 위조할 수 없게" 입력을 프레이밍하려고 상수로 선언돼 있다.

재수화가 끝나면 이 값이 `IndexState.LastHydratedSHA`로 저장되고, `planeHydrationStale(m, plane)`이 기록된 digest와 현재 digest를 비교한다. 기록이 비어 있으면(= 한 번도 수화한 적 없음) stale로 치지 않는다 — 그 경우는 카운트 비교가 이미 잡는다. 레코드 수는 그대로인데 내용만 바뀐 경우(수정·교체)는 카운트 비교로 잡히지 않으므로, 이 digest가 그 구멍을 메운다. `TestPlaneStateSHA`가 결정성과 평면 격리(다른 평면 변경에 영향받지 않음)를 고정한다.

---

## 3. 드리프트 판정 — `CheckDrift`

```go
type Drift struct {
    Detected    bool   `json:"detected"`
    Reason      string `json:"reason,omitempty"`
    Unavailable bool   `json:"unavailable"`
}
type DriftReport struct {
    Episodic  Drift `json:"episodic"`
    Knowledge Drift `json:"knowledge"`
}

// Service는 startup 검사 · 요청 진입 stat-gate · POST /v1/reindex의 계약이다.
func (s *service) CheckDrift(ctx context.Context) (DriftReport, error)
```

`rehydrate`는 code-standards §1의 exported 인터페이스 + unexported 구현체 쌍을 따른다: 공개 타입은 `Service`, 생성자는 `New(Config) (Service, error)` 하나, 구현체는 `service`. `Config.Store`·`Config.Clock`은 필수이고 `Index`/`Graph`는 nil이면 그 평면을 `Unavailable`로 보고한다(`TestNewRejectsIncompleteConfig`, `TestNewAcceptsNilDerivedStores`).

`CheckDrift`는 **아무것도 변경하지 않는다**. manifest를 읽고 두 평면을 각각 판정한다. manifest 읽기 실패만 error로 올라간다 — `errs.Wrap("rehydrate.CheckDrift", err)`이므로 원인의 Kind가 보존되고, 핸들러의 `apierr.From`이 그것을 상태코드로 바꾼다(§6).

드리프트 사유 문자열은 전부 `rehydrate.go` 상단 `const` 블록(`reasonIndexNotConfigured` … `reasonHotUnreadable`)에 한 번만 선언돼 있다 — "정직성 계약이므로 인라인 포맷으로 흩뿌리지 않는다"는 것이 코드 주석의 근거다.

### 3.1 episodic 판정 (`episodicDrift`) — 순서대로 평가

| 조건 | 결과 | `Reason` |
|---|---|---|
| `s.index == nil` | `Unavailable` | `opensearch not configured` |
| `index.Ping(ctx)` 실패 | `Unavailable` | `opensearch unreachable` |
| `index.DocCount(ctx, ProjectKey{})` error | `Detected` | `episodic index absent or uncountable` |
| `got != want` (`want` = manifest의 episodic 평면 `RecordCount` 합) | `Detected` | `indexed docs %d != manifest records %d` |
| dirty episodic 파일 수 > 0 | `Detected` | `%d dirty episodic file(s) await rehydration` |
| `planeHydrationStale(m, episodic)` (기록된 `LastHydratedSHA`가 비어있지 않을 때만) | `Detected` | `episodic hot content changed since last hydration` |

`want`/`dirty` 집계는 `planeTotals(m, plane)` — manifest 키 접두사로만 걸러낸다. `DocCount`는 zero-value `ProjectKey`를 넘겨 **전 프로젝트 합계**를 센다.

### 3.2 knowledge 판정 (`knowledgeDrift`)

| 조건 | 결과 | `Reason` |
|---|---|---|
| `s.graph == nil` | `Unavailable` | `neo4j not configured` |
| `graph.Ping(ctx)` 실패 | `Unavailable` | `neo4j unreachable` |
| hot 노드 수 집계 실패 | `Detected` | `hot knowledge unreadable: <err>` |
| `graph.NodeCount(ctx, ProjectKey{})` error | `Detected` | `knowledge graph uncountable` |
| `got != want` | `Detected` | `graph nodes %d != hot nodes %d` |
| dirty knowledge 파일 수 > 0 | `Detected` | `%d dirty knowledge file(s) await rehydration` |
| `planeHydrationStale(m, knowledge)` | `Detected` | `knowledge hot content changed since last hydration` |

episodic과 달리 기대값(`want`)을 manifest가 아니라 **hot 파일에서 직접 센다** — `hotNodeCount`가 `store.ListProjects` → `store.ReadKnowledge`를 돌며 `len(g.Nodes)`를 더하고, 실패하면 깨진 프로젝트 키를 원인에 실어 올린다(`/status`가 그것을 그대로 노출한다). 코드 주석은 그 이유를 "hotstore가 knowledge 레코드를 어떻게 집계하든 이 검사는 독립적으로 유지하려고"라고 밝힌다.

### 3.3 `Detected` ≠ `Unavailable`

- **`Detected`** = 파생 저장소는 살아 있는데 내용이 hot과 다르다 → **재수화 대상**.
- **`Unavailable`** = 파생 저장소에 닿을 수 없다 → **저하 상태일 뿐 재수화 대상이 아니다.** 재수화할 상대가 없기 때문이다.

`Server.Startup`은 이 구분을 그대로 따른다: `if !drift.Episodic.Detected && !drift.Knowledge.Detected { return }` — 두 저장소가 모두 죽어 `Unavailable`만 뜬 상태로 부팅하면 로그만 남기고 바로 서빙을 시작한다.

---

## 4. 세 개의 재수화 경로

| 경로 | 진입점 | 범위 | 트리거 조건 | 실패 처리 |
|---|---|---|---|---|
| startup | `Server.Startup(ctx)` ← `main.go` `run()`, `ListenAndServe` 직전 | 전체 | `CheckDrift`에서 어느 평면이든 `Detected` | 로그만. 절대 fatal 아님 |
| 요청 진입 stat-gate | `Server.statGate` → `server.Rehydrator.StatGate`(narrow interface, 구현체는 `rehydrate.Service`) | 프로젝트 1개 | 2초 디바운스 통과 + `needsRehydration` | `s.log.Warn`만, 요청은 계속 진행 |
| 강제 재수화 | `POST /v1/reindex[?verify=true]` → `RehydrateAll` | 전체 | 명시적 호출 | 200 + `Report.Failures`에 나열 |

### 4.1 startup

```go
func (s *Server) Startup(ctx context.Context)
```

`CheckDrift` → 로그(6개 필드: 평면별 `detected`/`reason`/`unavailable`) → `Detected`가 있으면 `RehydrateAll(ctx, false)` → 결과 로그 + 실패 항목별 `Warn`. `Server.rehydrator`가 nil이면 `"startup: rehydrator not configured; derived stores unmanaged"`만 남기고 반환한다. 어떤 경로에서도 프로세스를 죽이지 않는다 — 파생 저장소가 죽었다고 hot 쓰기까지 막을 수는 없기 때문이다.

### 4.2 요청 진입 stat-gate (2초 디바운스)

```go
const debounceInterval = 2 * time.Second   // internal/rehydrate/manifest.go
func (s *service) StatGate(ctx context.Context, key hotstore.ProjectKey) error  // internal/rehydrate/hydrate.go
```

1. `claimGate(key)` — `service.mu`가 지키는 `lastGate map[string]time.Time`에서 프로젝트별 마지막 통과 시각을 본다. `now.Sub(last) < 2s`면 즉시 반환(디바운스). 이 진입 자체가 뮤텍스 안에서 일어나므로 동시 버스트 32개 중 정확히 하나만 통과한다(`TestClaimGateAdmitsExactlyOneOfABurst`).
2. manifest를 읽고 `needsRehydration` = `hotChanged(...) || derivedLost(...)`로 값싼 신선도만 판단한다. 두 질문은 **독립**이다 — 정본과 파생물은 서로 없이도 움직이기 때문이다.
   - `hotChanged` (정본이 움직였나): 해당 평면 파일이 manifest에 `Dirty`로 표시돼 있거나, `store.FileInfo`의 **mtime이 `FileState.IndexedAt`보다 최신**이거나, hot 파일은 있는데 manifest가 **한 번도 본 적 없는** 파일(`!tracked`)일 때. dirty 검사 다음의 `FileInfo`가 실패하면(파일 없음·읽기 불가) 그 평면은 건너뛴다 — 수렴시킬 대상이 없다.
   - `derivedLost` (파생물이 잃어버렸나): `index.DocCount(key)` 또는 `graph.NodeCount(key)`가 manifest의 `RecordCount`보다 **적을** 때. 컨테이너가 교체된 상황의 서명이다.
3. 필요하면 `RehydrateProject(ctx, key)` — **해당 프로젝트만** 부분 재수화.

`derivedLost`에는 의도된 비대칭이 둘 있다(코드 주석이 근거를 담고 있다).

- **도달 불가는 결손이 아니다.** `DocCount`/`NodeCount`가 error를 내면 그 평면은 판정하지 않는다. 죽은 컨테이너를 상대로 재수화해봐야 헛돌 뿐이고, 그동안 쓰기는 degraded, 읽기는 503이다(§6).
- **결손만 세고 초과는 세지 않는다.** 파생물이 hot보다 많이 들고 있는 상태는 게이트의 트리거가 아니다. 그 수리는 `CheckDrift`(평면 단위 `got != want`)가 보고하고 `/v1/reindex`가 담당한다.

호출 지점은 **읽기 3곳뿐**이다:

| 핸들러 | 파일:라인 |
|---|---|
| `handleSearchEpisodes` | `internal/server/handlers_episodic.go:193` |
| `handleSearchKnowledge` | `internal/server/handlers_knowledge.go:358` |
| `handleKnowledgeGraph` | `internal/server/handlers_knowledge.go:410` |

쓰기 핸들러·`GET .../episodes/{id}`·문서 엔드포인트는 stat-gate를 타지 않는다. 쓰기는 자기 결과를 `markIndexed`/`markDirty`로 직접 manifest에 반영하므로 게이트가 필요 없다.

`Server.statGate`는 `Service.StatGate`의 error를 `Warn`으로만 남기고 요청을 계속 진행시킨다. 게이트 실패가 검색 자체를 막지는 않는다 — 재수화 문제는 `/v1/status`가 보고한다.

### 4.3 `POST /v1/reindex`

```go
func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request)   // handlers_ops.go
```

- `s.rehydrator`가 nil이면 503 `rehydrator unavailable`(`msgRehydratorUnavailable`).
- `?verify=true`(`paramVerify` / `valueTrue`)일 때만 전량 감사 모드로 돈다.
- `RehydrateAll`은 프로젝트 목록 조회 실패 외에는 error를 올리지 않는다. 부분 실패는 **200 + `Report.Failures[]`**로 정직하게 보고한다.

```go
type Report struct {
    EpisodesIndexed int      `json:"episodes_indexed"`
    NodesUpserted   int      `json:"nodes_upserted"`
    EdgesUpserted   int      `json:"edges_upserted"`
    Verified        bool     `json:"verified"`
    Failures        []string `json:"failures"`
}
```

---

## 5. 평면별 재구축 전략

두 평면은 재구축 방식이 다르다. 그 차이는 저장소 성격에서 온다.

| | episodic (OpenSearch) | knowledge (Neo4j) |
|---|---|---|
| 전체 경로 선행 작업 | `index.Drop(ctx)` — 인덱스 통째 삭제 | `graph.Clear(ctx)` — `MATCH (n:KnowledgeNode) DETACH DELETE n` |
| 스키마 보장 | `index.EnsureIndex(ctx)` — 전체·부분 두 경로 모두에서 명시 호출(nori 매핑 확인·복구) | `graph` 내부 `client.ensureSchema`가 upsert 직전 lazy 실행(id 인덱스 + fulltext 인덱스) |
| 재적재 | 프로젝트별 `ListEpisodes` → `IndexRecords` | 프로젝트별 `ReadKnowledge` → `UpsertNodes` → `UpsertEdges` |
| 부분 경로 선행 작업 | `index.DeleteProject(key)` — 프로젝트 범위 `_delete_by_query` | `graph.DeleteMissing(key, keep)` — 프로젝트 범위 `DETACH DELETE` (§5.1) |
| 멱등성 원천 | 인덱스를 버리므로 자명 | `MERGE (k:KnowledgeNode {id, ws, team, proj})` + 전 속성 `SET` |
| verify 감사 | `DocCount(p) == len(recs)` | `NodeCount(p) == len(g.Nodes)` |
| 실패 격리 | `Drop` 실패는 **비치명**(첫 수화 전 인덱스 부재가 정상) — Failures에만 기록하고 계속. `EnsureIndex` 실패는 평면 포기 | `Clear` 실패는 `full=false`로 표시하되 replay는 계속 |

### 5.1 왜 knowledge도 Clear가 필요한가

`MERGE`만으로는 **추가·수정**밖에 못 한다. hot에서 사라진 노드(purge 등)는 replay를 몇 번 돌려도 Neo4j에 남고, 그러면 `CheckDrift`가 `graph nodes N != hot nodes M`을 영원히 보고하며 어떤 재수화도 그것을 지울 수 없다. 그래서 **전체 경로(`RehydrateAll`)만** `Clear` 후 replay한다 — 코드 주석이 이 논리를 그대로 담고 있다(`TestRehydrateAllClearsStaleGraphNodes`가 고정).

`RehydrateProject`(부분 경로)는 `Drop`도 `Clear`도 호출하지 않는다. 한 프로젝트를 수렴시키려고 다른 프로젝트의 파생 데이터를 지울 수는 없기 때문이다(`TestRehydrateProjectDoesNotClearOtherProjects`). 대신 **프로젝트 범위로 좁힌 삭제**를 쓴다.

| | episodic | knowledge |
|---|---|---|
| 삭제 | `Index.DeleteProject(key)` — `_delete_by_query`가 `workspace/team/project` 필터에만 걸린다(`refresh=true`, `conflicts=proceed`). hot 레코드가 0건이어도 호출한다(전부 에이징된 상태가 바로 색인이 통째로 낡은 상태다). zero-value 키는 "전 프로젝트 삭제"가 되므로 `KindInvalid`로 거절한다 | `Graph.DeleteMissing(key, keep)` — `MATCH (n:KnowledgeNode {ws,team,proj}) WHERE NOT n.id IN $keep DETACH DELETE n`. `keep`이 nil이면 Cypher null이 되어 `IN`이 절대 false가 아니게 되므로 빈 슬라이스로 정규화한다 |
| 순서 | 삭제 → `IndexRecords` (전체 경로의 drop → bulk와 같은 모양) | upsert → 삭제 (중간 실패 시 데이터가 모자라는 쪽이 아니라 남는 쪽으로 기운다) |
| 없으면 생기는 일 | cold로 내려간 episode가 영원히 검색된다 — `commitManifest`가 dirty를 지워버려 그것을 고칠 신호마저 사라진다 | Neo4j 삭제가 실패한 purge 노드가 모든 replay를 살아남아 파생물에만 존재하는 콘텐츠가 된다(§0 원칙 1 위반) |

삭제가 실패하면 `ok[key]`를 세우지 않으므로 `touchFile`이 돌지 않고 **dirty가 그대로 남는다**(`TestRehydrateProjectKeepsDirtyWhenDeleteFails`) — 수렴하지 못한 평면이 깨끗하다고 기록되는 일은 없다.

### 5.2 수렴 후 manifest 커밋

`commitManifest(ctx, projects, epOK, knOK, epFull, knFull)` (`internal/rehydrate/manifest.go`)

- 프로젝트별 `epOK`/`knOK`가 true인 항목만 `touchFile` → `Dirty=false`, `IndexedAt=now`.
- **평면 전체가 수렴(`epFull`/`knFull`)했을 때만** `Indexes[key] = IndexState{LastHydratedSHA: PlaneStateSHA(nm, plane)}`. 일부 프로젝트가 실패했는데 평면 해시를 최신으로 찍어버리면 다음 `CheckDrift`가 문제를 못 보기 때문이다.
- 갱신은 `cloneManifest`로 깊은 복사한 뒤에 한다(불변 규칙). `hotstore` 쪽도 `UpdateManifest` 콜백에 이미 복사본을 넘기므로 공유 맵이 밖에서 변형될 경로가 없다.

`RehydrateProject`는 그 프로젝트가 곧 전부이므로 `epFull = epOK[key]`, `knFull = knOK[key]`를 그대로 넘긴다.

실패 누적과 반환 에러는 `failures` 헬퍼가 담당한다:

```go
func (f *failures) err(op string) error {
    if len(f.lines) == 0 {
        return nil
    }
    joined := errors.New(strings.Join(f.lines, failureSep))
    if f.unavailable {
        return errs.Unavailable(op, joined)
    }
    return errs.Internal(op, joined)
}
```

즉 파생 저장소가 아예 없어서 실패한 경우는 `KindUnavailable`, 그 밖의 실패는 `KindInternal`로 올라가고, 상세 문자열은 **cause에만** 남는다(응답 본문이 아니라 로그의 몫이다 — code-standards §2.2). `rep.Failures`는 에러 경로에서도 전부 채워진다(`TestRehydrateProjectDegraded`). stat-gate는 이 에러를 받아 `Warn`을 남긴다.

---

## 6. degraded 의미론 — degraded는 에러가 아니다

```go
// internal/server/degraded.go
const (
    degradedSearch = "search unavailable"
    degradedGraph  = "graph unavailable"
    degradedCold   = "cold storage unavailable"
    // degradedPromotion reports that the node was written but its provenance
    // episodes could not be marked consolidated, ...
    degradedPromotion = "provenance episodes not marked consolidated"
)

// 아예 만들지 못한 협력자는 degraded 노트가 아니라 503이다.
const (
    msgHotStoreUnavailable     = "hot store unavailable"
    msgDocumentsUnavailable    = "document pipeline unavailable"
    msgConsolidatorUnavailable = "consolidation unavailable"
    msgRehydratorUnavailable   = "rehydrator unavailable"
)
```

원칙: **hot 쓰기가 성공했으면 요청은 성공이다.** 파생 저장소 실패는 응답 본문의 `degraded` 배열로 보고할 뿐 상태코드를 바꾸지 않는다. 반대로 파생 저장소를 읽어야 하는 요청은 대신 지어낼 것이 없으므로 503으로 정직하게 실패한다. 이것이 code-standards §2.2 마지막 항목("degraded 모드: 쓰기 성공 + 파생 실패 = 에러 아님")을 코드로 옮긴 형태다.

### 6.1 쓰기 경로 — 200/201 + `degraded`

| 엔드포인트 | hot 쓰기 실패 | 파생 실패 시 상태코드 | `degraded` 값 | 부수 효과 |
|---|---|---|---|---|
| `POST /v1/{ws}/{team}/{proj}/episodes` | `apierr.From`이 도메인 Kind로 매핑(중복 id 409, 파일시스템 실패 500) | **201** | `["search unavailable"]` | `markDirty(episodic)` |
| `POST .../knowledge/nodes` | 동일 | **201** | `["graph unavailable"]` (+ 승격 실패 시 `"provenance episodes not marked consolidated"`) | `markDirty(knowledge)` |
| `POST .../knowledge/edges` | 동일 (끝점 없음 404) | **201** | `["graph unavailable"]` | `markDirty(knowledge)` |
| `PATCH .../knowledge/nodes/{id}` | 동일 (불법 전이 409) | **200** | `["graph unavailable"]` | `markDirty(knowledge)` |
| `DELETE .../knowledge/nodes/{id}?confirm=true` | 동일 (active 노드 409) | **200** | `["graph unavailable"]` | `markDirty(knowledge)` |

knowledge 계열은 전부 `s.mirrorKnowledge(ctx, key, nodes, edges)` 한 곳을 지난다(purge만 `graph.DeleteNode`를 직접 호출). `s.index`/`s.graph`가 **nil인 경우와 호출이 실패한 경우를 동일하게** 처리한다 — 둘 다 "파생물에 못 넣었다"는 같은 사실이다.

episodic 쪽에는 하나 더 있다: `convergeEpisodes`가 recall 카운터 갱신·consolidated 승격처럼 **hot을 제자리 수정한 뒤** 같은 레코드를 다시 색인하고 `markIndexed`를 호출한다. 이것이 없으면 hot sha만 바뀌어 `episodic hot content changed since last hydration`이 영구히 남는다(§3.1의 마지막 줄).

degraded 쓰기 응답 예:

```json
{
  "success": true,
  "data": {
    "record": { "id": "01JD…", "kind": "observation", "text": "…", "consolidated": false },
    "degraded": ["search unavailable"]
  }
}
```

`CreateEpisodeResponse.Degraded` / `NodeResponse.Degraded` / `EdgeResponse.Degraded` / `PurgeNodeResponse.Degraded`는 모두 `json:"degraded,omitempty"` — 정상일 때는 필드 자체가 사라진다.

### 6.2 읽기 경로 — 503

| 엔드포인트 | 503 조건 | 본문 `error.message` |
|---|---|---|
| `GET .../episodes/search` | `s.index == nil` 또는 `errors.Is(err, errs.ErrUnavailable)` | `search unavailable` |
| `GET .../knowledge/search` | `s.graph == nil` 또는 `errors.Is(err, errs.ErrUnavailable)` | `graph unavailable` |
| `GET .../knowledge/graph` | 위와 동일 | `graph unavailable` |
| `POST .../documents` | `s.documents == nil` / `s.archiver == nil` | `document pipeline unavailable` / `cold storage unavailable` |
| `POST /v1/consolidate` | `s.consolidator == nil` | `consolidation unavailable` |
| `POST /v1/reindex` | `s.rehydrator == nil` | `rehydrator unavailable` |

503 응답 봉투 — `Envelope.Error`는 자유 문장이 아니라 `*apierr.Error`(`{code, message, details?}`)다:

```json
{
  "success": false,
  "data": null,
  "error": { "code": "unavailable", "message": "search unavailable" }
}
```

이 503들은 `apierr.From`이 아니라 `respond.go`의 `unavailable(msg)` 헬퍼가 만든다. 이유는 주석에 있다: **쓰기가 달았을 degraded 노트와 글자 그대로 같은 문구**를 503 본문이 재사용하게 하려는 것이다. 저장소가 낸 원인은 `WithCause(err)`로 붙여 로그에만 남는다.

`KindUnavailable`을 붙이는 주체는 저장소 패키지 자신이다.

- `internal/search`: `transport.go`의 `unreachable`(전송 오류·5xx)과 `indexAbsent`(인덱스 404). **인덱스 부재를 `[]Hit{}`가 아니라 unavailable로 보고**하는 것이 핵심이다 — 그래야 "히트 없음"과 "파생물이 사라짐"이 응답에서 갈린다.
- `internal/graph`: `bolt.go`의 `mapErr`가 `neo4j.IsConnectivityError` 또는 `context.DeadlineExceeded`일 때만 `errs.Unavailable`을 붙인다.

진짜 질의 오류는 `errs.Internal`로 남아 그대로 500이 된다 — 저하와 버그를 섞지 않는다.

### 6.3 예외: 문서 ingest는 cold-first라 저하가 없다

`POST .../documents`는 `s.archiver`가 없으면 **503 `cold storage unavailable`**로 거절하고, `document.service.Ingest`도 같은 지점에서 `errs.Unavailable`을 낸다. §6 파이프라인의 2단계(S3 blob 업로드)는 best-effort가 아니라 계약이기 때문이다. 반면 청크 색인(`service.indexChunks`)과 문서 노드 미러(`service.upsertNode`)는 실패해도 `service.reportDegraded`가 `IngestResult.Degraded`에 노트를 얹고 `slog.Warn` + `Store.MarkDirty`만 한 뒤 ingest는 201로 끝난다.

### 6.4 저하 → 수렴의 고리

```
파생 upsert 실패 → markDirty(plane) → manifest Files[key].Dirty = true
   ├─ CheckDrift: "N dirty …file(s) await rehydration" → /status가 노출
   ├─ needsRehydration: hotChanged(dirty=true) → 다음 stat-gate가 그 프로젝트만 부분 재수화
   └─ RehydrateAll: 전체 재수화 후 commitManifest가 dirty 해제
```

블랙박스 `TestScenario07_DegradedMode`가 이 고리를 통째로 검증한다: `docker stop dj-memory-opensearch` → 쓰기 201 + degraded → hot에 레코드 존재 확인 → 검색 503 → `docker start` → `POST /v1/reindex` → 같은 레코드가 검색에 잡힘.

---

## 7. `/v1/status` — 정직성 보고

```go
type StatusReport struct {
    Drift               rehydrate.DriftReport `json:"drift"`
    Unconsolidated      int                   `json:"unconsolidated"`
    StaleUnconsolidated int                   `json:"stale_unconsolidated"`
    ManifestUpdatedAt   time.Time             `json:"manifest_updated_at"`
    DirtyFiles          []string              `json:"dirty_files"`
    S3                  S3SyncStatus          `json:"s3"`
    Degraded            []string              `json:"degraded"`
}
```

- `Drift`는 매 요청 `CheckDrift`를 실제로 돌려 채운다. `driftReport`가 그 래퍼다 — `s.rehydrator`가 없거나 `CheckDrift`가 실패하면 두 평면 모두 `Unavailable`에 `rehydrator not configured` / `drift check failed`를 적는다. **"드리프트 없음"으로 조용히 넘기지 않는다.**
- `Degraded[]`는 `handleStatus`가 도는 순서대로 쌓인다: ① 드리프트 `Unavailable` → `search unavailable`, ② `graph unavailable`, ③ 미통합 집계 실패 → `unconsolidated count unavailable` / `unconsolidated count incomplete: <project>`, ④ cold 도달 불가(또는 archiver 미구성) → `cold storage unavailable`.
- `DirtyFiles`는 manifest 키 그대로(`"episodic/ws/team/proj"`)를 정렬해 노출한다.
- cold 도달성은 `coldReachable`이 `archiver.FetchBlob(ctx, emptySHA256)`으로 **GET** 프로브를 한다. HEAD는 존재하지 않는 버킷과 없는 키를 구분하지 못해 "없는 버킷을 reachable로 보고"하기 때문. `errors.Is(err, errs.ErrNotFound)`면 버킷은 살아 있는 것으로 판정한다.
- `/status`는 hot store 자체가 nil일 때(503 `hot store unavailable`)와 manifest를 못 읽을 때(`apierr.From` → 500)를 빼면 **항상 200으로 답한다.** 부분 실패는 `Degraded`에 실어 보낸다.

---

## 8. 알려진 공백과 주의점

### 8.1 컨테이너 소멸은 이제 게이트가 감지한다 — 다만 디바운스 창이 남는다

stat-gate의 판단 근거는 hot 파일의 dirty/mtime **과** 파생 저장소의 결손 두 가지다(§4.2 `derivedLost`). 그래서 컨테이너가 죽었다가 빈 상태로 돌아왔고 그동안 hot 파일이 바뀌지 않았더라도, 다음 검색 요청이 게이트를 통과하면 `DocCount`/`NodeCount`가 manifest보다 적은 것을 보고 그 프로젝트를 부분 재수화한다. 예전에 이 자리에 있던 "빈 인덱스가 조용한 0건으로 보인다"는 공백은 두 겹으로 닫혔다.

1. 게이트가 결손을 직접 본다.
2. 게이트가 돌지 못했더라도 `search.Search`가 인덱스 404를 `indexAbsent`(= `KindUnavailable`)로 보고하므로 응답은 **200 + 빈 배열이 아니라 503**이다.

남은 창은 좁고 명시적이다.

- **디바운스 창(2초)**: 직전 2초 안에 같은 프로젝트의 게이트가 통과했다면 다음 요청은 게이트를 건너뛴다. 이때 episodic 읽기는 위 (2)에 따라 503으로 정직하게 실패한다.
- **초과(surplus)**: 파생물이 hot보다 **많이** 들고 있는 상태는 게이트 트리거가 아니다(§4.2). 감지는 `GET /v1/status`의 `CheckDrift`(평면 단위 `got != want`)가 하고, 수리는 `POST /v1/reindex`가 한다.

블랙박스 시나리오 5도 그래서 `/v1/reindex?verify=true`를 **명시적으로** 호출한다. 운영 절차는 [10-operations](10-operations.md)를 따른다.

### 8.2 `episodic index absent or uncountable` 분기는 실제로는 인덱스 부재로 도달하지 않는다

`episodicDrift`의 해당 분기 주석은 "인덱스 자체가 없어 셀 수 없는 경우"를 말하지만, 실제 `search` 클라이언트의 `DocCount`는 404를 **`(0, nil)`로 정상 반환**한다("No index means zero docs — a legitimate drift signal, not an error"). 따라서 신선한 컨테이너의 인덱스 부재는 `got != want`(=`indexed docs 0 != manifest records N`)로 잡히고, 해당 분기는 404가 아닌 비정상 상태코드·응답 디코드 실패·`Ping` 직후 끊긴 전송에서만 발동한다.

- 부수 효과: hot이 완전히 비어 있으면(`want == 0`) 인덱스가 없어도 드리프트가 아니다 — 재구성할 것이 없으므로 의도된 동작이다.
- 같은 404 관용은 `Search`에는 적용되지 않는다(§6.2). "0건"이 정답인 곳과 "파생물이 없다"가 정답인 곳을 코드가 구분한다.

### 8.3 문서 ingest 직후에는 일시적으로 드리프트가 보고된다

`document` 패키지는 색인/미러가 **성공한 경우** `markIndexed`에 해당하는 갱신을 하지 않는다(`MarkDirty`만 호출한다). hot 파일 sha는 바뀌었는데 `Indexes[*].LastHydratedSHA`는 그대로이므로, 업로드 직후 `/status`가 `episodic hot content changed since last hydration`(및 knowledge 쪽 동일 사유)을 보고한다. 다음 검색 요청의 stat-gate가 mtime 변화를 보고 해당 프로젝트를 부분 재수화하면서 수렴하지만, 그 한 번은 잉여 재색인이다.

### 8.4 `⚠️ 설계 문서와 차이`

| 항목 | `architecture-v2.md` §5 | 실제 코드 |
|---|---|---|
| manifest 파일 상태 | `{sha256, record_count, indexed_at}` | `dirty bool`이 추가되어 있다(§1의 dirty 마크와 통합된 형태) |
| startup 재수화 조건 | "인덱스 존재·doc count, 노드 count를 manifest와 대조 → 불일치 시 전체 재수화" | `Detected`일 때만 재수화. `Unavailable`(핑 실패)만인 경우 로그만 남기고 서빙 시작 |
| knowledge 재수화 | "`MERGE` 멱등 upsert" | 순수 MERGE는 어느 경로에도 없다. 전체 경로는 `graph.Clear` **후** MERGE replay, 부분 경로는 MERGE replay **후** `graph.DeleteMissing` 조정이다 — MERGE만으로는 hot에서 사라진 노드를 지울 수 없기 때문(§5.1) |
| 요청 진입 게이트 트리거 | "manifest 나이 2초 디바운스 후, dirty 마크 또는 파일 mtime 변화" | 디바운스는 **프로젝트별 마지막 게이트 통과 시각** 기준(`service.lastGate`)이며 manifest의 `UpdatedAt` 나이를 보지 않는다. 신호도 dirty·mtime(+미추적 파일)에 더해 **파생 저장소 결손**(`derivedLost`)이 하나 더 있다 |

---

## 9. 검증

| 대상 | 테스트 (`internal/rehydrate/`) |
|---|---|
| 생성자 계약 | `TestNewRejectsIncompleteConfig`, `TestNewAcceptsNilDerivedStores` |
| 드리프트 판정 | `TestCheckDrift` (table-driven, `rehydrate_test.go`) |
| 파생 저장소 도달 불가·집계 실패 | `TestDriftWhenDerivedStoresUnreachable`, `TestCheckDriftManifestReadFailure`, `TestKnowledgeDriftHotReadFailure`, `TestGraphUncountableIsDrift`, `TestHotNodeCountReadFailure` |
| 전체 재수화 + verify | `TestRehydrateAll`, `TestRehydrateAllVerifyMismatch`, `TestRehydrateAllDegraded` |
| 부분 실패 격리 | `TestRehydrateAllDropFailureIsNonFatal`, `TestRehydrateAllEnsureIndexFailure`, `TestRehydrateAllEpisodeListingFailureIsPerProject`, `TestRehydrateAllEdgeUpsertFailure`, `TestRehydrateKnowledgeFailures`, `TestRehydrateAllProjectListingFailure`, `TestRehydrateAllManifestWriteFailureIsReported` |
| 제거 수렴 (§5.1) | `TestRehydrateAllClearsStaleGraphNodes`, `TestRehydrateProjectDropsEpisodesNoLongerHot`, `TestRehydrateProjectDropsNodesNoLongerHot`, `TestRehydrateProjectWithEmptyHotStillClearsIndex`, `TestRehydrateProjectKeepsDirtyWhenDeleteFails`, `TestRehydrateProjectDoesNotClearOtherProjects` |
| 부분 재수화 · 저하 | `TestRehydrateProject`, `TestRehydrateProjectDegraded`(4종 table), `TestRehydrateProjectWithNoHotContent`, `TestRehydrateProjectHotKnowledgeReadFailure`, `TestRehydrateProjectManifestFailureIsReported` |
| 2초 디바운스 · 게이트 | `TestStatGate`(dirty 트리거 → `debounceInterval/2` 무시 → `debounceInterval+1s` 재수렴 / mtime 트리거 / 미추적 파일 트리거 / 신선하면 no-op / 실패는 `errs.ErrUnavailable`), `TestStatGateManifestFailure`, `TestClaimGateAdmitsExactlyOneOfABurst`(동시 32개 중 1개만 통과) |
| 평면 해시 · 인덱스 키 | `TestPlaneStateSHA`, `TestIndexKeyFor` |
| 컨테이너 소멸 → 등가 회복 | `TestScenario05_Rehydration` — `docker compose down -v --remove-orphans && up -d --build` → Neo4j 노드 0 확인 → `/status` 드리프트 → `/v1/reindex?verify=true` → nori 검색 히트와 supersede 체인이 이전과 동일 |
| 저하 모드 왕복 | `TestScenario07_DegradedMode` |
| 정직성 총괄 | `TestScenario08_StatusHonesty` — 최종 상태에서 드리프트 clean, `dirty_files` 비어 있음, `degraded` 비어 있음 |

실행: 단위는 `make test`(컨테이너·AWS 불필요), 블랙박스는 `make blackbox`(실제 컨테이너 + 실제 S3).

---

## 10. 코드 위치

| 개념 | 파일 | 심볼 |
|---|---|---|
| 비영속 컨테이너 정의 | `deploy/docker-compose.yml` | `opensearch`, `neo4j` (볼륨 없음) |
| 재수화 계약 · 좁은 인터페이스 | `internal/rehydrate/rehydrate.go` | `Service`, `service`, `New(Config)`, `HotStore` / `EpisodeIndex` / `KnowledgeGraph` / `Clock` |
| 드리프트 판정 | `internal/rehydrate/rehydrate.go` | `CheckDrift`, `episodicDrift`, `knowledgeDrift`, `hotNodeCount`, `reason*` 상수 |
| 실패 누적 · 에러 Kind 결정 | `internal/rehydrate/rehydrate.go` | `failures`, `failures.add`, `failures.addUnavailable`, `failures.err` |
| 전체 재수화 | `internal/rehydrate/hydrate.go` | `RehydrateAll`, `rehydrateEpisodicAll`, `rehydrateKnowledgeAll` |
| 부분 재수화 · 게이트 | `internal/rehydrate/hydrate.go` | `RehydrateProject`, `rehydrateProjectEpisodic`, `rehydrateProjectKnowledge`, `StatGate`, `claimGate`, `needsRehydration`, `hotChanged`, `derivedLost` |
| manifest 키 · 해시 · 디바운스 · 커밋 | `internal/rehydrate/manifest.go` | `IndexKeyFor`, `PlaneStateSHA`, `planeHydrationStale`, `planeTotals`, `debounceInterval`, `commitManifest`, `touchFile`, `cloneManifest` |
| manifest 자료구조 · 영속화 | `internal/hotstore/manifest.go` | `Manifest`, `FileState`, `IndexState`, `MarkDirty`, `updateFileStateLocked`, `updateManifestLocked` |
| manifest 파일 키 · 평면 | `internal/hotstore/key.go` | `ManifestFileKey`, `Plane`, `PlaneEpisodic`, `PlaneKnowledge` |
| 원자 쓰기 · 정규 직렬화 · stat | `internal/hotstore/file.go` | `writeFileAtomic`, `marshalCanonical`, `FileInfo` |
| 서버 측 narrow interface | `internal/server/deps.go` | `Rehydrator`(`CheckDrift`/`RehydrateAll`/`StatGate`), `EpisodeIndex`, `KnowledgeGraph`, `ColdArchive`, `Clock` |
| 부팅 시 대조 | `internal/server/startup.go` | `Server.Startup` |
| 저하 표기 · manifest 부기 · 게이트 호출 | `internal/server/degraded.go` | `degradedSearch`, `degradedGraph`, `degradedCold`, `degradedPromotion`, `msg*Unavailable`, `markIndexed`, `markDirty`, `statGate` |
| 저하 쓰기 (episodic) | `internal/server/handlers_episodic.go` | `handleCreateEpisode`, `convergeEpisodes`, `CreateEpisodeResponse.Degraded` |
| 저하 쓰기 (knowledge) | `internal/server/handlers_knowledge.go` | `mirrorKnowledge`, `promoteProvenance`, `handleCreateNode`, `handleCreateEdge`, `handlePatchNode`, `handlePurgeNode` |
| 503 읽기 경로 | `internal/server/handlers_episodic.go`, `handlers_knowledge.go` | `handleSearchEpisodes`, `handleSearchKnowledge`, `handleKnowledgeGraph` |
| 상태 보고 · 강제 재수화 | `internal/server/handlers_ops.go` | `StatusReport`, `handleStatus`, `driftReport`, `coldReachable`, `handleReindex` |
| 봉투 · 전송 에러 변환 | `internal/server/respond.go`, `internal/server/apierr/apierr.go` | `Envelope`, `writeAPIError`, `unavailable`, `apierr.From`, `mappingFor` |
| 의미 에러 · degraded 신호 | `internal/errs/errs.go` | `KindUnavailable`, `ErrUnavailable`, `Unavailable(op, cause)`, `Wrap` |
| 저하 분류 (episodic) | `internal/search/transport.go`, `internal/search/query.go` | `unreachable`, `indexAbsent`, `Search`(404 → unavailable), `DocCount`(404 → 0) |
| 인덱스 재구축 원시 연산 | `internal/search/index.go`, `internal/search/bulk.go` | `EnsureIndex`, `Drop`, `DeleteProject`, `IndexRecords` |
| 저하 분류 · 그래프 재구축 (knowledge) | `internal/graph/bolt.go`, `internal/graph/queries.go` | `mapErr`, `Clear`, `DeleteMissing`, `UpsertNodes`, `UpsertEdges`, `NodeCount` |
| 의존성 배선 | `cmd/memory-mcp/main.go` | `rehydrate.New(rehydrate.Config{Store, Index, Graph, Clock, Logger})`, `server.Config.Rehydrator`, `srv.Startup(ctx)` |
