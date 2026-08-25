# 03 — 메모리 생명주기: 상태기계와 hot→cold 에이징

episodic은 append-only이고 유한하며, knowledge는 영구이고 상태 전이만 한다 — 두 평면의 수명 규칙, 이동 자격 판정식, 미통합 레코드 불변식, 그리고 절대 뒤집히지 않는 S3 put → hot 삭제 → 인덱스 삭제 순서를 코드 기준으로 규정한다.

| 항목 | 내용 |
|---|---|
| 관련 코드 | `internal/knowledge/{knowledge.go,lifecycle.go}` · `internal/consolidate/{consolidate.go,age.go,cluster.go,report.go}` · `internal/config/config.go` · `internal/server/{handlers_knowledge.go,handlers_ops.go,handlers_episodic.go,respond.go}` · `internal/errs/errs.go` · `internal/server/apierr/apierr.go` · `internal/cold/{archive.go,keys.go}` · `internal/hotstore/episode.go` |
| 관련 스펙 | [01-overview](01-overview.md) · [02-storage-model](02-storage-model.md) · [04-episodic-search](04-episodic-search.md) · [05-knowledge-graph](05-knowledge-graph.md) · [07-rehydration](07-rehydration.md) · [08-http-api](08-http-api.md) · [10-operations](10-operations.md) · [11-testing](11-testing.md) · 인덱스: [README](README.md) |
| 상태 | 코드 반영 완료. 남은 설계 문서·주석과의 차이는 §8에 명시 |

![위쪽은 knowledge 노드의 active·archived·deprecated 상태기계와 purge 게이트, 아래쪽은 episodic 레코드가 ingest에서 S3 월별 배치까지 가라앉는 에이징 타임라인. 빨간 X는 코드가 막는 전이](assets/03-lifecycle.svg)

---

## 1. 두 평면의 수명 계약

| | episodic | knowledge |
|---|---|---|
| 변경 모델 | append-only. 정정은 새 레코드 | 비파괴 개정 — supersede 체인 + 상태 전이 |
| 수명 | **유한** — consolidation 실행 시 cold로 가라앉는다 | **영구** — hot에 항상 상주 |
| hot에서 사라지는 경로 | `ageProject` (S3 아카이브 성공 후) | `DELETE .../knowledge/nodes/{id}?confirm=true` 뿐 |
| 자동 삭제 | 없음. `consolidated=true`인 레코드만 이동 | 없음. 상태 전이는 값을 바꿀 뿐 레코드를 지우지 않는다 |
| 트리거 | `POST /v1/consolidate` (유일) | HTTP 요청 시점 (즉시) |
| 삭제 후 복구 | S3 `{yyyy-mm}.json`에서 조회 가능 | S3 `latest.json` + `snapshots/{ts}.json` + 버킷 versioning |
| 도메인 코드 | `internal/episodic/record.go` | `internal/knowledge/{knowledge.go,lifecycle.go}` |

두 평면 모두 **서버가 판정하지 않는다**. episodic의 `consolidated` 플래그도, knowledge의 supersede/deprecate 판정도 호출 에이전트가 결정한다. `Record.Consolidated`의 주석이 그 계약이다 (`internal/episodic/record.go:95-99`):

```go
// Consolidated marks the record as distilled into knowledge; a
// precondition for cold archival (§3). Never inferred by the server: it is
// set only where the agent states the distillation itself, by naming this
// record in the provenance of a POST .../knowledge/nodes (§3, §0
// principle 2).
```

즉 서버가 이 플래그를 쓰는 유일한 자리는 §4.2의 `promoteProvenance`이며, 그것도 **에이전트가 한 선언(provenance 지목)의 결과를 기록**할 뿐이다.

---

## 2. knowledge 상태기계

이 절에서 **도메인(`internal/knowledge`)이 내는 거절은 전부 `*errs.Error`**다. 도메인은 HTTP를 모르고 `errs.Kind`만 붙이며(`internal/errs/errs.go:24-30`), 그 Kind가 상태코드로 바뀌는 자리는 `apierr.From`이 참조하는 고정 표 `mappingFor` 하나뿐이다 (`internal/server/apierr/apierr.go:51-64`): `KindInvalid`→400, `KindNotFound`→404, `KindConflict`→409, `KindUnavailable`→503, 나머지→500.

전송 계층만 아는 거절 — 잘못된 `op`, 빠진 `confirm`, ULID가 아닌 path 파라미터 — 은 도메인을 거치지 않고 `badRequest`/`notFound`/`unavailable`이 `*apierr.Error`를 직접 만든다 (`internal/server/respond.go:78-94`). 핸들러가 도메인 에러의 센티널로 분기하는 자리는 §5 어휘를 그대로 재사용해 503을 다시 쓰거나(`errors.Is(err, errs.ErrUnavailable)` — `handlers_knowledge.go:361`·`:413`, `handlers_episodic.go:196`) cold 폴백으로 넘어가는(`errs.ErrNotFound` — `handlers_episodic.go:319`) 경우뿐이다. `errs.ErrConflict`로 분기하는 핸들러는 하나도 없다 — 이 절의 409는 전부 `apierr.From`이 만든다.

### 2.1 합법 전이

전이표는 `internal/knowledge/lifecycle.go:31-42`의 switch 함수 하나가 전부다 (패키지 전역 맵을 두지 않는다 — code-standards §1.1의 "mutable package state 금지"):

```go
func allowedTargets(from State) []State {
	switch from {
	case StateActive:
		return []State{StateArchived, StateDeprecated}
	case StateArchived:
		return []State{StateActive, StateDeprecated}
	case StateDeprecated:
		return []State{StateActive}
	default:
		return nil
	}
}
```

| from \ to | active | archived | deprecated |
|---|---|---|---|
| **active** | no-op(허용) | O | O (reason 필수) |
| **archived** | O (부활) | no-op(허용) | O (reason 필수) |
| **deprecated** | O (부활) | **X** | no-op(허용, reason 필수) |

- `deprecated → archived`가 유일하게 막히는 전이다. 다이어그램의 빨간 점선이 이것.
- 동일 상태 전이는 `Transition`의 `n.State != target` 가드에 걸리지 않으므로 **합법**이며, 그래도 `Updated = now`는 갱신된다 (`lifecycle.go:54-74`).
- 알 수 없는 `target`(예: `"retired"`)은 `ValidState`에서 걸려 **`KindConflict`(409)**로 거절된다 (`lifecycle.go:55-58`). 다만 HTTP 경로에서는 `targetState()`가 먼저 400으로 끊으므로 실제로는 도달하지 않는다.
- `target == StateActive`면 `SupersededBy`를 `""`로 초기화한다 (`lifecycle.go:69-71`) — active 노드는 `superseded_by`를 가질 수 없다는 `Node.Validate` 불변식(`knowledge.go:189-191`)을 만족시키기 위한 것. `Supersedes` 리스트는 그대로 남는다.
- `Transition`은 입력 노드를 절대 변형하지 않고 복사본을 반환한다 (`TestTransitionDoesNotMutateInput`, `internal/knowledge/lifecycle_test.go:79`).

### 2.2 deprecate는 reason이 필수

```go
if target == StateDeprecated && strings.TrimSpace(reason) == "" {
	return Node{}, errs.Conflict(opTransition, EntityNode, n.ID,
		"deprecating requires a reason — why is it wrong?")
}
```

(`lifecycle.go:63-66`) HTTP 계층에서 한 번 더 막는다 — `PatchNodeRequest.targetState()`(`internal/server/handlers_knowledge.go:496-513`)가 `op=="deprecate"` 또는 `set_state`의 target이 `deprecated`인데 `reason`이 빈 문자열이면 **400**으로 끊는다.

- 두 게이트의 판정 기준이 미묘하게 다르다: 전송 계층은 `req.Reason == ""`, 도메인은 `strings.TrimSpace(reason) == ""`. 따라서 `reason: "   "`(공백만)은 400을 통과해 도메인까지 내려가고 거기서 `KindConflict` → **409**가 된다. 도메인 게이트가 죽은 코드가 아니라는 뜻이다.
- `op`는 `set_state` | `deprecate` 두 값만 허용, 그 외는 400 (`handlers_knowledge.go:498-508`).
- 불법 전이(`deprecated → archived`)는 `Transition`의 `KindConflict`가 `apierr.From`을 거쳐 **409 Conflict**가 된다 (`handlers_knowledge.go:487-490`). 공개 메시지는 도메인이 쓴 문장 그대로 — `"cannot transition deprecated -> archived"` (`apierr.publicMessage`, `apierr.go:153-160`).
- 존재하지 않는 노드는 404(`notFound("node not found")`, `handlers_knowledge.go:474-477`), ULID가 아닌 id는 400.
- **reason은 저장되지 않는다.** `knowledge.Node`에 reason 필드가 없고 핸들러도 어디에 기록하지 않는다 — 게이트 역할만 한다 (§8-1).

### 2.3 supersede — 비파괴 개정

`Supersede(g, newNode, supersedes, now)` (`lifecycle.go:153-210`)는 새 그래프를 반환하는 순수 함수다. 입력 그래프는 변형되지 않는다.

1. `newNode.ID`가 이미 그래프에 있으면 `KindConflict`(→ **409**). 서버가 ULID를 발급하므로 실제로는 도달하지 않는다.
2. `supersedes`를 `dedupe`하고, 자기 자신을 가리키면 `KindInvalid`(→ 400), 없는 id면 `KindNotFound`(→ **404**).
3. **패자**: `SupersededBy = newNode.ID`, 상태가 `active`면 `archived`로 이동 (이미 `deprecated`면 그대로 둔다), `Updated = now`.
4. **승자**: `State = active`, `SupersededBy = ""`, `Supersedes = targets`, `Created`가 zero면 `now`, `Updated = now`.
5. 패자마다 `Edge{From: 승자, To: 패자, Rel: supersedes, Provenance: 승자 provenance의 복사본, Confidence: supersedeConfidence(=1.0)}`를 추가한다. 같은 `(from,to,rel)` 엣지가 이미 있으면 건너뛴다 (`hasEdge`).
6. `supersedes`가 비어 있으면 단순 append.

호출부는 `POST .../knowledge/nodes` 하나뿐이고, 그것도 `UpdateKnowledge` 클로저 **안에서** 호출된다 (`handlers_knowledge.go:161-182`) — 읽기·판정·쓰기가 한 락 안에 있어야 동시 생성이 노드를 잃지 않는다. 노드는 항상 `state=active`, 서버가 주입된 `IDGenerator`로 부여한 ULID(`s.ids.GenerateAt(now.UnixMilli())`, `handlers_knowledge.go:133-138`)로 생성되며, 클라이언트가 상태나 id를 지정할 방법은 없다.

체인은 `superseded_by`를 따라 최신 노드까지 올라가고, `supersedes` 리스트와 `supersedes` 엣지로 반대 방향도 추적된다. Neo4j 순회는 [05-knowledge-graph](05-knowledge-graph.md) 참조.

### 2.4 purge — confirm 게이트 + 생명주기 게이트

`DELETE /v1/{ws}/{team}/{proj}/knowledge/nodes/{id}?confirm=true` (`handlers_knowledge.go:536-587`):

| 단계 | 동작 |
|---|---|
| 게이트 | `confirm != "true"`면 **400** — `"purge requires confirm=true (§3 — S3 versioning is the backstop)"`. id 검증보다 **먼저** 돈다: 파괴적 호출은 파괴적 이유로 먼저 거절된다 |
| id 검증 | ULID 아니면 400 |
| hot 쓰기 | `knowledge.Purge`(`lifecycle.go:97-131`)가 `UpdateKnowledge` 클로저 안에서 전부 수행 — 조회·게이트·제거가 같은 락 안에 있다 |
| 생명주기 게이트 | `Purge`가 `CanPurge`(archived·deprecated만 허용, `lifecycle.go:79-81`)를 적용해 active 노드를 `KindConflict` → **409**로 거절 — `"purge requires state archived or deprecated — archive or deprecate the node first"`. §3의 완충층을 건너뛰는 직접 삭제는 없다 |
| 없는 노드 | `Purge`의 `FindNode`가 `KindNotFound` → **404** |
| 제거 범위 | 노드 제거 + **인접 엣지 전부 제거**(`e.From == id \|\| e.To == id`) + **매달린 참조 복구**(다른 노드의 `superseded_by`를 `""`로, `supersedes`에서 해당 id 제거 — `withoutID`) |
| 파생물 | `Graph.DeleteNode` best-effort — 실패하거나 graph가 nil이면 `degraded: ["graph unavailable"]` + manifest dirty 마크 |
| 응답 | `PurgeNodeResponse{purged_id, removed_edges, degraded}` (`handlers_knowledge.go:61-66`) |

매달린 참조 복구가 게이트만큼 중요하다: 복구하지 않으면 남은 노드가 사라진 id를 계속 가리켜 개정 체인이 끊기고, 그 노드가 `Node.Validate`를 통과하지 못하게 될 수도 있다 (`lifecycle.go:92-96` 주석).

purge는 hot에서만 지운다. 백스톱은 S3 — 버킷 versioning과 consolidation이 남긴 `knowledge/.../latest.json`·`snapshots/{ts}.json`이다. 스냅샷이 없는 갓 만든 노드가 사라지는 일은 생명주기 게이트가 막는다: purge하려면 먼저 `archived`나 `deprecated`로 전이해야 하고, 그 전이 자체가 에이전트의 명시적 판정이다.

### 2.5 상태가 읽기에 미치는 영향

Neo4j 전문검색은 기본적으로 `active`만 반환한다 — `graph.Client.Search`의 Cypher 한 줄이 그 필터다 (`internal/graph/queries.go:97-121`, 필터는 `:104`):

```cypher
AND ($includeArchived OR node.state = 'active')
```

`GET .../knowledge/search?include_archived=true`로만 archived·deprecated가 노출된다 (opt-in). `GET .../knowledge/graph`(이웃 순회, `queries.go:126-148`)에는 이 필터가 없다.

---

## 3. episodic 에이징

### 3.1 append-only가 코드에서 강제되는 방식

- 라우터(`internal/server/server.go:177-190`)에 episode용 `PUT`/`PATCH`/`DELETE`가 **아예 없다**. `POST /episodes`, `GET /episodes/search`, `GET /episodes/{id}` 셋뿐 (`server.go:178-180`).
- `POST /episodes`는 `Consolidated: false`를 하드코딩한다 (`handlers_episodic.go:113-122`). 요청 바디(`CreateEpisodeRequest`, `handlers_episodic.go:20-27`)에 해당 필드 자체가 없다.
- `occurred_at`이 zero면 `now`로 채운다 (`handlers_episodic.go:109-112`). id는 서버가 주입된 `IDGenerator`로 부여한다(`s.ids.GenerateAt(now.UnixMilli())`).
- hot 파일을 in-place로 고치는 유일한 경로는 `hotstore.Client.UpdateEpisodes`(`internal/hotstore/hotstore.go:64-66`)이고, 서버는 이걸 **두 곳에서만** 쓴다 — recall 통계 갱신(`bumpRecall`, `handlers_episodic.go:215-234`)과 증류 승격(`promoteProvenance`, `handlers_knowledge.go:212-247`). 둘 다 값 필드만 바꾸고 레코드를 추가·삭제하지 않는다.
- `UpdateEpisodes`는 fn이 id를 바꾸면 `errs.Internal`(→500)을 반환해 id 불변성을 지키고, 요청한 id가 하나라도 없으면 `KindNotFound`를 반환한다 (`internal/hotstore/episode.go:81-121`).

### 3.2 `ageEligible` — 이동 자격 판정

```go
func ageEligible(recs []recordView, now time.Time, ttlDays int, fileBytes int64, maxBytes int64, maxRecords int) []string
```

순수 함수이며 (`internal/consolidate/age.go:111-144`), 에이징 자격을 판정하는 곳은 여기 하나뿐이다. 패키지 밖으로 노출되지 않는다 — 판정식은 `consolidate`의 내부 규칙이고, 외부에는 `Service.Run`의 결과(`Report`)만 나간다.

**(1) TTL 규칙**

```
cutoff  = now - ttlDays일          // now.AddDate(0, 0, -ttlDays)
자격    = Consolidated == true  AND  OccurredAt <= cutoff
```

경계는 닫혀 있다 — `!rec.OccurredAt.After(cutoff)`(`age.go:124`)이므로 **정확히 ttlDays 지난 레코드는 이동한다** (`age_test.go:47-56`: "exactly ttl days old is eligible" / "one day short of ttl stays hot").

**(2) 압박 규칙** (`pressureQuota`, `age.go:148-162`)

```
need = 0
if maxRecords > 0 && recordCount > maxRecords:
    need = recordCount - maxRecords
if maxBytes > 0 && fileBytes > maxBytes && recordCount > 0:
    keep = maxBytes * recordCount / fileBytes     // 정수 나눗셈, 레코드 평균 크기 추정
    need = max(need, recordCount - keep)
```

그 다음 오래된 consolidated부터 `len(eligible) >= need`가 될 때까지 채운다 (`age.go:129-135`). TTL로 이미 자격을 얻은 레코드도 이 쿼터에 산입된다 (`age_test.go:91-100`).

- `recordCount`는 **미통합 레코드를 포함한 전체 건수**(`len(recs)`)다. 즉 압박 목표치는 전체 기준으로 계산되지만 실제로 뺄 수 있는 건 consolidated뿐이라, consolidated가 모자라면 쿼터는 조용히 미달로 끝난다 (`age_test.go:81-90`).
- `maxBytes`/`maxRecords`가 0 이하면 해당 압박 검사는 비활성 (`age_test.go:109-119`).
- `ageProject`는 `FileInfo`가 실패하면 `fileBytes = noFilePressure(=0)`으로 두어 바이트 압박을 끈다 (`age.go:15-17`, `23-26`) — 없는 파일은 압박을 만들지 않는다.

**(3) 정렬** — 반환 id는 `(OccurredAt, ID)` 오름차순, 즉 오래된 것부터다 (`byOccurrenceThenID`, `age.go:166-174`). ULID 타이브레이크 덕분에 같은 시각 레코드도 실행마다 같은 순서로 나간다.

| 상황 | 결과 |
|---|---|
| `consolidated=true`, 40일 전 | 이동 |
| `consolidated=true`, 5일 전, 압박 없음 | 잔존 |
| `consolidated=false`, 40일 전 | **잔존 (영구)** |
| `consolidated=false`, 압박 초과 | **잔존 (영구)** |
| 파일 8 MiB / 상한 4 MiB, 레코드 4건 전부 consolidated | 오래된 2건 이동 (`keep = 4*4/8 = 2`, `age_test.go:69-80`) |

### 3.3 불변식 — 미통합 episode는 절대 자동 삭제되지 않는다

`ageEligible`은 후보 집합을 만들 때부터 `rec.Consolidated`인 것만 `consolidated` 슬라이스에 담는다 (`age.go:114-119`). 따라서 TTL이든 압박이든, **어떤 경로로도 `consolidated=false`인 레코드의 id는 반환되지 않는다.** 증류 없이 버리는 삭제는 존재하지 않는다.

대신 정직하게 계속 노출한다 — `GET /v1/status`의 `countUnconsolidated`(`internal/server/handlers_ops.go:170-195`)가 전 프로젝트를 스캔해:

- `unconsolidated`: `consolidated=false`인 전체 건수
- `stale_unconsolidated`: 그중 `OccurredAt < now - EpisodicTTLDays`인 건수 — "TTL이 지났는데도 이동하지 못한 것"

TTL 기준 시각은 주입된 `Clock`으로 계산한다 (`s.clock.Now().UTC().Add(-time.Duration(s.ttlDays) * hoursPerDay)`, `handlers_ops.go:177`). 프로젝트 목록 조회가 실패하면 카운트를 숨기지 않고 `degraded: ["unconsolidated count unavailable"]`로 알린다.

블랙박스 시나리오 6이 이 불변식을 실물로 검증한다: 40일 된 레코드 2건 중 consolidated인 것만 S3로 내려가고 미통합 건은 hot 파일에 남아야 하며(`blackbox_test.go:455-461`), 시나리오 8의 `/status`가 `stale_unconsolidated == 1`이어야 한다 (`test/blackbox/blackbox_test.go:387-486`, `:599-600`).

### 3.4 이동 순서 — S3 put → hot 삭제 → 인덱스 삭제

`ageProject`(`internal/consolidate/age.go:22-86`)는 자격 id를 `cold.ArchiveMonth(rec.OccurredAt)`(UTC `2006-01`, `cold/keys.go:70-72`) 기준으로 월별 배치(`batchByMonth`, `consolidate.go:233-240`)로 묶고, 월 키를 오름차순으로 처리한다. 각 배치마다:

| # | 단계 | 코드 | 실패 시 |
|---|---|---|---|
| 3a | `ColdArchiver.ArchiveEpisodes` — S3 `{yyyy-mm}.json` put | `age.go:61-67` | `Failures`에 `archive {month}` 기록 후 **다음 월로 continue — 로컬은 아무것도 건드리지 않는다** |
| 3b | `HotStore.RemoveEpisodes` — hot 파일에서 제거 | `age.go:69-75` | `Failures`에 `hot-remove {month}` 기록 후 continue — **인덱스 삭제도 하지 않는다.** cold 사본만 존재하는 안전한 방향이고, 다음 실행이 멱등하게 재아카이브한다 |
| 3c | `EpisodeIndexer.DeleteRecords` — OpenSearch 문서 삭제 | `age.go:78-84`, `deleteFromIndex`는 `age.go:90-95` | `Failures`에 `index-delete {month}` 기록 + `MarkDirty(PlaneEpisodic)` — 드리프트는 재수화가 수렴시킨다 |

`MovedEpisodes`는 3b가 성공한 시점에만 증가한다 (`age.go:76`).

**멱등성 근거**

- `cold` 클라이언트의 `ArchiveEpisodes`는 download → merge(레코드 id 기준 중복 제거) → upload이고, 결과를 id로 정렬해 쓴다 (`internal/cold/archive.go:23-59`). 없는 객체를 받으면 빈 슬라이스로 시작하므로(`downloadBatch`, `archive.go:64-81`) 첫 아카이브와 재시도가 같은 코드 경로다 — 부분 실패 후 재실행해도 중복 레코드가 생기지 않는다.
- `RemoveEpisodes`는 없는 id와 없는 파일을 조용히 건너뛰고, 실제로 지워진 게 없으면 파일을 다시 쓰지 않는다 — manifest churn 없음 (`internal/hotstore/episode.go:125-150`).
- `Archiver`가 nil(=S3 초기화 실패)이면 아예 `"age: {ws/team/proj}: cold storage unavailable"` 실패만 기록하고 로컬은 손대지 않는다 (`age.go:43-46`, `report.go:86-90`).

**테스트 근거** — `internal/consolidate/run_test.go`가 공유 호출 로그(`calls`)로 순서를 직접 단언한다:

| 테스트 | 단언 |
|---|---|
| `TestRunHappyPath` (`run_test.go:310-318`) | 월별로 `archive:` → `remove:` → `index-delete:` 순서 고정 |
| `TestRunS3FailureBlocksLocalDeletion` (`:321-351`) | S3 실패 시 `remove:`/`index-delete:` 로그가 **하나도** 없고, hot 4건 전부 잔존 |
| `TestRunHotRemoveFailureSkipsIndexDelete` (`:353-374`) | hot 삭제 실패 시 인덱스 삭제로 넘어가지 않음, `MovedEpisodes == 0` |
| `TestRunIndexFailureIsDegraded` (`:376-396`) | 인덱스 삭제 실패는 dirty 마크 + 실패 보고, 이동은 유효(`MovedEpisodes == 2`) |

---

## 4. consolidation 파이프라인

트리거는 `POST /v1/consolidate` **하나**다. 바디는 `ConsolidateRequest{projects []string, dry_run bool}`(`internal/server/handlers_ops.go:61-66`)이며 `projects`는 `"ws/team/proj"` 문자열이다. 핸들러가 `parseProjectString`으로 각 문자열을 `hotstore.ProjectKey`로 변환해 `consolidate.Options.Projects []hotstore.ProjectKey`에 담는다 (`handlers_ops.go:218-226`) — 파이프라인 안쪽은 검증된 키만 본다. 비우면 `HotStore.ListProjects`가 돌려주는 전 프로젝트가 대상이다.

파이프라인의 진입점은 `consolidate.Service` 인터페이스이고(`consolidate.go:69-75`), 구현체 `service`는 `New(Config)` 하나로만 만들어진다(`consolidate.go:126-142`). `Config`는 `Store`/`Index`/`Archiver`/`Clock`/`Logger`/`TTLDays`를 받고, `Store`·`Clock`·양수 `TTLDays`가 없으면 `New`가 `KindInvalid`로 실패한다 (`consolidate.go:98-110`). `Index`·`Archiver`는 nil이어도 부팅한다 — 파생물이 죽었다고 파이프라인이 못 뜨면 안 되고, 대신 해당 단계가 `Failures`에 정직하게 남는다.

### 4.1 5단계 (`service.Run`, `consolidate.go:147-209`)

프로젝트 루프 안에서 1~4를 돌고, 루프가 끝난 뒤 entity 통계를 정렬하고 5를 한 번 수행한다.

| 단계 | 하는 일 | 코드 |
|---|---|---|
| 1. 후보 제안 | 미통합 episode를 (정규화된) 공유 entity + 24시간 윈도우로 그리디 클러스터링해 `Candidate{project, entities, episode_ids, from, to}` 목록 반환 | `consolidate.go:169-176` → `cluster.go:29-74`, `candidateWindow = 24h` (`cluster.go:15`) |
| 2. entity 통계 | 프로젝트의 **모든** 레코드에 대해 entity를 trim+lowercase 정규화하고 건수·쌍별 co-occurrence를 누적 | `cluster.go:98-137` |
| 3. 에이징 | §3.4 | `age.go:22-86` |
| 4. 스냅샷 | `ReadKnowledge` → `SnapshotKnowledge`로 `latest.json`과 `snapshots/{yyyymmdd}T{hhmmss}Z.json` 두 개를 업로드, 두 키 모두 보고 | `consolidate.go:213-229`, `cold/archive.go:114-130`, 키 형식은 `cold/keys.go:46-58` |
| 5. manifest 갱신 | 항등 함수로 `UpdateManifest`를 호출해 `UpdatedAt`만 밀어 올린다 → `/status`가 실행 사실을 반영 | `consolidate.go:195-202` |

클러스터링 규칙 상세: 레코드를 `(occurred_at, id)`로 정렬한 뒤, 각 레코드는 **가장 최근에 만들어진** 호환 클러스터(24시간 이내 + entity 교집합 존재)에 붙고, 없으면 새 클러스터를 만든다 (`findCluster`, `cluster.go:78-94`). entity가 없는 레코드는 entity가 없는 클러스터하고만 묶인다. `Candidate.From`/`To`는 경계를 넘어가므로 UTC로 정규화해서 내보낸다 (`cluster.go:68-70`).

### 4.2 서버가 하는 일 / 에이전트가 하는 일

| | 서버 (결정적) | 호출 에이전트 |
|---|---|---|
| 증류 | **하지 않는다** — 요약·LLM·임베딩 없음 | episode를 읽고 요약해 knowledge 노드/엣지를 만든다 |
| `consolidated=true` | 추론하지 않는다. 에이전트가 `provenance`로 지목한 episode에만 그 선언의 결과로 세팅한다 (`promoteProvenance`) | `POST .../knowledge/nodes`의 `provenance`에 증류한 episode를 적어 증류를 선언한다 |
| 후보 묶기 | entity·시간 클러스터를 제안 | 어느 후보를 증류할지 선택 |
| supersede 판정 | 요청받은 대로만 수행 | 무엇이 무엇을 대체하는지 결정 |
| deprecate 사유 | 비어 있으면 거부 | 왜 틀렸는지 문장을 제공 |
| 에이징·스냅샷 | 규칙대로 실행하고 결과를 보고 | 실행 시점을 결정 (`POST /v1/consolidate`) |

`consolidate` 패키지 doc 주석이 이 계약을 그대로 적어 두었다 — *"Distillation itself (summarize → knowledge) is the calling agent's job, never the server's."* (`consolidate.go:1-5`)

**증류 루프를 닫는 자리** — `handleCreateNode`가 hot 쓰기에 성공한 뒤 `promoteProvenance`(`handlers_knowledge.go:212-247`)를 호출한다:

1. `provenance`가 비어 있으면 아무 것도 하지 않는다.
2. 프로젝트의 hot episode를 읽어 **지목되었고 아직 `consolidated=false`인 것만** 골라낸다 — 이미 consolidated인 레코드를 다시 쓰면 hot 파일만 헛되이 갱신된다.
3. `UpdateEpisodes`로 `Consolidated = true`를 찍고, `convergeEpisodes`로 색인까지 맞춘다 (`consolidated`는 색인 필드라 hot만 바꾸면 검색이 "아직 미증류"라고 답한다).
4. 이 프로젝트의 hot에 없는 id(이미 cold로 내려갔거나 타 프로젝트)는 **건너뛴다** — 이미 쓰인 노드를 실패시키지 않는다.
5. 전 과정이 best-effort다. 실패는 `degraded: ["provenance episodes not marked consolidated"]`로 응답에 실려 나가고(`internal/server/degraded.go:16-19`), 노드 생성은 201로 성공한다.

### 4.3 보고와 실패 처리

```go
type Report struct {
	Candidates []Candidate  `json:"candidates"`
	Entities   []EntityStat `json:"entities"`
	// MovedEpisodes counts records shifted to cold; ArchiveKeys lists the
	// {yyyy-mm}.json objects written.
	MovedEpisodes int      `json:"moved_episodes"`
	ArchiveKeys   []string `json:"archive_keys"`
	// SnapshotKeys lists knowledge latest+snapshot objects written.
	SnapshotKeys []string `json:"snapshot_keys"`
	// Failures lists per-step errors; a failure never blocks other steps.
	Failures []string `json:"failures"`
}
```

(`internal/consolidate/report.go:50-61`)

- 슬라이스는 `newReport()`가 전부 non-nil로 초기화한다 (`report.go:65-73`) — 조용한 실행도 `null`이 아니라 `[]`로 나간다.
- **단계·프로젝트 실패는 나머지를 중단시키지 않는다.** `Failures`에 `"{step}: {ws/team/proj}: {err}"` 형태로 쌓일 뿐이다 (`addFailure`, `report.go:76-78`). 프로젝트를 특정할 수 없는 단계(manifest)는 `"{step}: {err}"` (`addStepFailure`). 단계 이름은 `report.go:11-20`에 한 번만 선언된다.
- `Run`이 error를 반환하는 경우는 `ListProjects` 실패 하나뿐 (`consolidate.go:151-157`) → 그때만 HTTP 500.
- 전부 실패해도 응답은 **200**이며 `data.failures`로 보고한다. 정직성 우선(fail-fast 아님)이 의도된 설계다. 실패가 하나라도 있으면 주입된 로거에 `WarnContext`로 남긴다 (`consolidate.go:204-207`).
- 핸들러는 `ArchiveKeys`/`SnapshotKeys`가 비어 있지 않을 때만 `lastArchiveAt`/`lastSnapshotAt`을 갱신한다(`recordColdActivity`, `statusMu` 보호, `handlers_ops.go:239-249`). 이 값이 `/status`의 `s3.last_archive_at`·`s3.last_snapshot_at`으로 나간다.
- `Consolidator`가 nil이면 503(`handlers_ops.go:209-212`), 프로젝트 셀렉터 문자열이 잘못되면 400.

### 4.4 dry_run

`dry_run: true`면 후보·entity 통계는 계산하고 `MovedEpisodes`에 **이동했을 건수**만 채운 뒤 S3 업로드·hot 삭제·인덱스 삭제·스냅샷·manifest 갱신을 전부 건너뛴다 (`age.go:39-42`, `consolidate.go:186-188`·`195-202`). 카운트는 `Archiver`가 nil인지 확인하기 **전에** 매겨지므로, S3가 없어도 "규칙상 몇 건이 나갈지"는 답할 수 있다. `TestRunDryRun`(`run_test.go:427-453`)이 호출 로그가 완전히 비어 있고 manifest도 건드리지 않음을 단언한다.

---

## 5. 상수와 환경변수

`internal/config/config.go:43-56`이 임계값의 단일 출처다.

| 상수 | 값 | 의미 |
|---|---|---|
| `DefaultEpisodicTTLDays` | `30` | consolidated episode의 cold 이동 기준 일수 |
| `MaxProjectFileBytes` | `5 << 20` = 5 MiB (5,242,880 B) | 프로젝트 episodic 파일 압박 임계 |
| `MaxProjectRecords` | `5000` | 프로젝트 episodic 레코드 수 압박 임계 |

| env | 기본값 | 비고 |
|---|---|---|
| `DJ_MEMORY_EPISODIC_TTL_DAYS` | `30` | 양의 정수만. 파싱 실패나 0 이하면 `resolveTTLDays`가 `KindInvalid`를 반환해 프로세스가 부팅하지 않는다 (`config.go:204-216`, `172-175`) |
| `DJ_MEMORY_HOME` | `~/.local/dj-memory` | hot 루트 |
| `DJ_MEMORY_USERNAME` | OS 사용자 | S3 키 프리픽스 |
| `DJ_MEMORY_S3_BUCKET` / `DJ_MEMORY_S3_REGION` | `vms-memory-mcp` / `ap-northeast-2` | cold 목적지 |

`MaxProjectFileBytes`·`MaxProjectRecords`는 env로 조정할 수 없다 — `ageProject`가 `config` 상수를 직접 참조한다 (`age.go:34`). TTL만 주입 가능하며 `consolidate.New(consolidate.Config{..., TTLDays: cfg.EpisodicTTLDays})`로 `service.ttlDays`에 들어간다 (`cmd/memory-mcp/main.go:169-176`). 같은 값이 `server.Config.EpisodicTTLDays`로도 들어가 `/status`의 stale 계산에 쓰인다 (`main.go:193-207`).

---

## 6. 에이징 후에도 깨지지 않는 것

episode가 cold로 내려가도 id는 불변이므로 knowledge의 `provenance: ["episode_id"]` 링크는 유효하다.

- `GET /v1/{ws}/{team}/{proj}/episodes/{id}`는 hot 조회가 `errs.ErrNotFound`면 `ColdArchive.FetchArchivedEpisode`로 폴백한다 (`handlers_episodic.go:299-334`, 폴백은 `:323-333`). 아카이버가 nil이면 404 + `"episode not in hot store; cold archive unavailable"`. 그 외 에러는 폴백 없이 `apierr.From`으로 그대로 나간다 (`:319-322`).
- `FetchArchivedEpisode`는 `{username}/episodic/{ws}/{team}/{proj}/` 프리픽스를 리스팅해 **최신 월부터** 배치를 내려받아 id를 찾는다 (`cold/archive.go:85-110`, 프리픽스는 `cold/keys.go:42-44`). 월 배치가 많아질수록 선형으로 느려진다.
- **검색은 hot 범위만 본다.** 에이징 3c에서 OpenSearch 문서를 지우므로 아카이브된 episode는 `GET /episodes/search`에 잡히지 않는다. 블랙박스 시나리오 6이 "aged=0 hits, stale은 여전히 검색됨"으로 확인한다 (`blackbox_test.go:477-484`).

---

## 7. 상태 흐름 요약 (요청 → 결과)

| 요청 | 결과 | 실패 코드 |
|---|---|---|
| `POST .../knowledge/nodes` (supersedes 없음) | `state=active` 노드 append | 400(검증), 500(hot 쓰기), 503(store 없음) |
| `POST .../knowledge/nodes` (supersedes 있음) | 승자 active + 패자 archived + `supersedes` 엣지 | 404(대상 없음), 409(id 중복 — 서버 발급이라 사실상 불가) |
| `POST .../knowledge/nodes` (provenance 있음) | 위 + 지목된 hot episode를 `consolidated=true`로 승격 | 승격 실패는 201 + `degraded` |
| `PATCH .../knowledge/nodes/{id}` `{op:"deprecate", reason}` | `deprecated`로 전이 | 400(reason 빈 문자열), 404, 409(불법 전이 · 공백만 있는 reason) |
| `PATCH ...` `{op:"set_state", state:"active"}` | 부활 + `superseded_by` 초기화 | 409(`deprecated→archived` 같은 불법 조합) |
| `DELETE .../knowledge/nodes/{id}?confirm=true` | hot에서 노드+인접 엣지 제거 + 매달린 참조 복구 | 400(confirm 없음), 404, **409(active 노드 — 먼저 archive/deprecate)** |
| `POST /v1/consolidate` | 후보·통계·에이징·스냅샷·manifest | 400(프로젝트 셀렉터), 500(`ListProjects` 실패), 503(consolidator 없음) |
| `GET /v1/status` | `unconsolidated`, `stale_unconsolidated`, `s3.last_archive_at` 등 | 503(hot store 없음), 500(manifest 불가) |

---

## 8. ⚠️ 설계 문서와 차이

| # | 코드 실제 | 문서/주석 |
|---|---|---|
| 1 | `reason`은 `Transition`의 게이트로만 쓰이고 **어디에도 저장되지 않는다.** `Node`에 필드가 없고 감사 로그를 쓰는 코드도 없다 | `lifecycle.go:47-48` 주석 "which the caller records in the PATCH audit" |
| 2 | 압박 임계는 5 MiB(`5 << 20` = 5,242,880 B)와 5,000건. env로 조정 불가 | 설계 §3.1은 "파일 > 5MB 또는 5,000건" (MB vs MiB) |
| 3 | 스냅샷은 dry-run이 아닌 **모든 실행에서 모든 프로젝트**에 대해 수행되며, knowledge 그래프가 비어 있어도 `latest.json` + `snapshots/{ts}.json` 2개를 쓴다. 스냅샷 정리(retention) 로직은 `internal/cold` 어디에도 없다 | 설계 §3.1 "knowledge: 이동 없음 — 스냅샷 백업만"과 방향은 같지만, 무조건·무제한 누적이라는 점은 미규정 |
| 4 | 에이징 트리거는 `POST /v1/consolidate` 뿐. `cmd/memory-mcp/main.go`에 플래그 파싱(`flag`·`os.Args`) 자체가 없다 | 설계 §4 "또는 `--consolidate` CLI" |
| 5 | 압박 쿼터 `need`는 `len(recs)`(미통합 포함) 기준으로 계산되므로, 미통합이 다수인 프로젝트에서는 압박이 영구히 해소되지 않는다 | 설계 §3.1은 "오래된 consolidated부터"만 규정하고 쿼터 미달을 다루지 않는다 |
| 6 | `allowedTargets`는 `active → deprecated` **직행**과 `archived/deprecated → active` **부활**을 허용하고, `Transition`은 동일 상태 전이도 허용하며 `Updated`를 갱신한다 | 설계 §3의 화살표는 `active ──supersede──► archived ──deprecate──► deprecated` 일직선뿐 — 직행·부활·self-loop가 그려져 있지 않다 |

> 이전 판에 있던 "`consolidated=true`를 세팅하는 HTTP 경로가 없다", "`knowledge.CanPurge`는 테스트에서만 호출된다" 두 항목은 코드로 해소되어 삭제했다. 각각 §4.2의 `promoteProvenance`와 §2.4의 `knowledge.Purge` 게이트가 그 자리를 메운다.

---

## 9. 코드 위치

| 개념 | 파일 | 심볼 |
|---|---|---|
| 상태·전이 정의 | `internal/knowledge/knowledge.go:49-55`, `:139-146` · `internal/knowledge/lifecycle.go:31-42` | `State`, `StateActive/Archived/Deprecated`, `ValidState`, `allowedTargets` |
| 전이 실행 | `internal/knowledge/lifecycle.go:54-74` | `Transition` (거절은 `errs.Conflict` → 409) |
| purge 자격·실행 | `internal/knowledge/lifecycle.go:19-21`, `:79-143` | `msgPurgeNotBuffered`, `CanPurge`, `Purge`, `withoutID` |
| supersede 체인 | `internal/knowledge/lifecycle.go:23-25`, `:153-241` | `supersedeConfidence`, `Supersede`, `FindNode`, `hasEdge`, `dedupe` |
| 노드 불변식 | `internal/knowledge/knowledge.go:130-225` | `ValidNodeKind/ValidState/ValidTrust/ValidRel`, `Node.Validate`, `Edge.Validate` |
| 에러 의미 → 상태코드 | `internal/errs/errs.go:24-30`, `:60-64` · `internal/server/apierr/apierr.go:51-64`, `:125-160` | `Kind*`, `Err*` 센티널, `mappingFor`, `From`, `publicMessage` |
| 전송 전용 거절 | `internal/server/respond.go:78-94` | `badRequest`, `notFound`, `unavailable` |
| 증류 승격 | `internal/server/handlers_knowledge.go:212-247` · `internal/server/degraded.go:16-19` | `promoteProvenance`, `degradedPromotion` |
| 상태 전이 HTTP | `internal/server/handlers_knowledge.go:443-513` | `handlePatchNode`, `PatchNodeRequest`, `targetState` |
| purge HTTP | `internal/server/handlers_knowledge.go:536-587` | `handlePurgeNode`, `PurgeNodeResponse` |
| supersede HTTP | `internal/server/handlers_knowledge.go:113-191` | `handleCreateNode` |
| episodic append-only | `internal/server/handlers_episodic.go:83-140` · `internal/episodic/record.go:80-105` | `handleCreateEpisode`, `Record.Consolidated` |
| 이동 자격 판정 | `internal/consolidate/age.go:99-174` | `ageEligible`, `recordView`, `pressureQuota`, `byOccurrenceThenID` |
| 에이징 실행·순서 | `internal/consolidate/age.go:22-95` | `ageProject`, `deleteFromIndex` |
| 파이프라인 5단계 | `internal/consolidate/consolidate.go:40-142`, `:147-240` · `report.go:50-99` | `HotStore`/`EpisodeIndexer`/`ColdArchiver`/`Clock` narrow interface, `Service`, `Config`, `New`, `service.Run`, `snapshotProject`, `batchByMonth`, `Options`, `Report` |
| 후보 클러스터링 | `internal/consolidate/cluster.go:15-94` | `candidateWindow`, `proposeCandidates`, `findCluster` |
| entity 통계 | `internal/consolidate/cluster.go:98-146` | `normalizeEntities`, `entityAccumulator` |
| 스냅샷 | `internal/consolidate/consolidate.go:213-229` · `internal/cold/archive.go:114-130` | `snapshotProject`, `SnapshotKnowledge` |
| 월별 아카이브 포맷 | `internal/cold/archive.go:23-110` · `internal/cold/keys.go:36-72` | `ArchiveEpisodes`, `downloadBatch`, `FetchArchivedEpisode`, `EpisodeArchiveKey`, `ArchiveMonth` |
| hot 제거·수정 | `internal/hotstore/episode.go:81-150` | `UpdateEpisodes`, `RemoveEpisodes` |
| 임계값·TTL | `internal/config/config.go:43-56`, `:204-216` | `DefaultEpisodicTTLDays`, `MaxProjectFileBytes`, `MaxProjectRecords`, `resolveTTLDays` |
| 미통합 노출 | `internal/server/handlers_ops.go:33-51`, `:170-195` | `StatusReport`, `countUnconsolidated` |
| consolidate HTTP | `internal/server/handlers_ops.go:208-249` | `handleConsolidate`, `ConsolidateRequest`, `recordColdActivity` |
| cold 폴백 조회 | `internal/server/handlers_episodic.go:299-334` | `handleGetEpisode` |
| 생명주기 테스트 | `internal/knowledge/lifecycle_test.go:18-383` · `internal/consolidate/{age_test.go,run_test.go}` · `internal/server/handlers_knowledge_test.go:455-917` · `test/blackbox/blackbox_test.go:387-486` | `TestTransition`, `TestSupersede`, `TestPurgeRequiresBuffer`, `TestAgeEligible`, `TestRunS3FailureBlocksLocalDeletion`, `TestPurgeNodeRequiresConfirm`, `TestCreateNodeMarksProvenanceEpisodesConsolidated`, `TestScenario06_Consolidation` — [11-testing](11-testing.md) |
