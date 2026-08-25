# 05 — Knowledge 그래프: Neo4j와 supersede 체인

"지금 무엇이 참인가"를 담는 knowledge 평면의 노드·엣지 모델, Neo4j로 실제로 나가는 Cypher, 그리고 삭제 대신 개정으로 진실을 갱신하는 supersede 체인.

| 항목 | 내용 |
|---|---|
| 관련 코드 | `internal/knowledge/knowledge.go` · `internal/knowledge/lifecycle.go` · `internal/graph/{graph,queries,convert,bolt}.go` · `internal/server/handlers_knowledge.go` · `internal/server/knowledge_graph.go` |
| 관련 스펙 | [01-overview](01-overview.md) · [02-storage-model](02-storage-model.md) · [03-lifecycle](03-lifecycle.md) · [04-episodic-search](04-episodic-search.md) · [06-documents](06-documents.md) · [07-rehydration](07-rehydration.md) · [08-http-api](08-http-api.md) · [11-testing](11-testing.md) · 인덱스: [README](README.md) |
| 상태 | 구현 완료. 본 문서는 위 Go 코드를 읽고 실측해서 쓴 것이며, 설계 문서([architecture-v2.md](../design/architecture-v2.md) §2·§2.2·§3·§5·§7)와 어긋나는 지점은 ⚠️로 표시했다 |

![knowledge 그래프의 supersede 체인과 provenance 링크](assets/05-knowledge-graph.svg)

## 1. 왜 그래프인가

episodic이 시간축의 append-only 로그라면 knowledge는 **관계축**이다. 실제로 필요한 질문 세 가지가 전부 관계를 따라가는 질문이다.

| 질문 | 필요한 연산 | 구현 |
|---|---|---|
| "memory-mcp에 대해 아는 게 뭐지?" | entity를 중심으로 n-hop 이웃 | `graph.Client.Neighborhood` |
| "이 사실은 뭘 뒤집은 거지?" | supersedes 링크를 양방향으로 추적 | `graph.Client.SupersedeChain` |
| "이건 어디서 알게 됐지?" | 노드 → 유래 episode 역참조 | `provenance` 속성 |

앞의 둘은 가변 홉 순회라서 관계형 조인으로 풀면 홉마다 쿼리가 는다. 세 번째는 그래프 순회가 아니라 **id 배열 참조**로 풀려 있다(§7 참고). 그래서 Neo4j를 쓰되, 정본은 여전히 hot JSON 한 덩어리(`knowledge.Graph`)이고 Neo4j는 **언제든 버리고 `MERGE`로 재생하는 파생물**이다 — 이 비대칭이 아래 모든 설계 결정을 지배한다.

`internal/knowledge`는 순수 도메인 패키지다: I/O도, 앰비언트 시계도, HTTP도 없다(호출자가 `now`를 넘긴다). `internal/graph`는 code-standards §1 규약대로 **exported `Client` 인터페이스 + unexported `client` 구조체 + 생성자 `New(Config) (Client, error)`** 한 쌍으로 되어 있고, 드라이버는 `runner`라는 소비자 측 narrow interface 뒤에 숨어 있어 단위 테스트가 fake로 갈아끼운다.

## 2. 노드와 엣지 — `internal/knowledge`의 실제 타입

`knowledge.Graph`는 프로젝트 한 개의 knowledge 문서 전체다. hot 경로는 `knowledge/{ws}/{team}/{proj}.json`이고, `hotstore.Client.UpdateKnowledge`가 락을 쥔 채 읽고-결정하고-원자 교체한다([02 · 저장 모델](02-storage-model.md)).

```go
type Graph struct {
    Nodes []Node `json:"nodes"`
    Edges []Edge `json:"edges"`
}
```

### 2.1 Node

| 필드 | 타입 | 의미 |
|---|---|---|
| `id` | `string` | ULID. 불변, 노드 정체성의 유일한 근거 |
| `kind` | `NodeKind` | `entity` \| `fact` \| `lesson` \| `preference` \| `document` |
| `name` / `body` | `string` | 표제 / 서술. 둘 다 fulltext 색인 대상 |
| `aliases` | `[]string` | 별칭. Neighborhood의 중심 매칭과 fulltext에 함께 쓰인다 |
| `state` | `State` | `active` \| `archived` \| `deprecated` |
| `trust` | `Trust` | `user-stated` \| `agent-inferred` \| `imported` |
| `supersedes` | `[]string` | 이 노드가 대체한 노드 id들 |
| `superseded_by` | `string` | 이 노드를 대체한 노드 id. active면 반드시 `""` |
| `provenance` | `[]string` | 유래 episode id들 |
| `created` / `updated` | `time.Time` | |
| `review_after` | `string` | RFC3339 또는 `""` |

값 집합은 전부 **닫혀 있다**. `ValidNodeKind` / `ValidState` / `ValidTrust` / `ValidRel`이 §2.2의 열거값을 담고 있고, 그 밖의 문자열은 거부된다. 넷 다 패키지 레벨 맵이 아니라 **`switch` 함수**인데, 런타임에 값 집합이 변형될 수 없게 하려는 것이다(code-standards §1.1 "패키지 전역 변수·싱글턴·`init()` 부작용 금지"; 코드 주석도 이 절을 인용한다). 그리고 넷 다 **exported**다 — 어휘의 주인은 knowledge 패키지 하나이고, HTTP 경계는 사본을 갖는 대신 이 함수들을 그대로 호출한다(code-standards §4 "중복 헬퍼는 삭제가 아니라 통합").

`Node.Validate()`가 강제하는 불변식:

- `id`는 ULID (`ulid.Valid`, 26자 Crockford base32)
- `kind` / `state` / `trust`는 닫힌 집합 소속
- `name`은 `strings.TrimSpace` 후 비어 있으면 안 됨 (`body`는 비어도 됨)
- **`state == active`이면 `superseded_by`는 반드시 빈 문자열** — active인데 대체된 노드는 존재할 수 없다
- `superseded_by`가 있으면 그것도 ULID
- `review_after`는 RFC3339이거나 빈 문자열

위반은 전부 `errs.Invalid(op, entity, msg)` — 즉 `*errs.Error{Kind: KindInvalid}`이고, 호출자는 `errors.Is(err, errs.ErrInvalid)`로 비교한다. 패키지 전용 센티넬(`ErrInvalidNode` 같은 것)은 없다. `op`는 `knowledge.Node.Validate`, `entity`는 exported 상수 `knowledge.EntityNode`(`"knowledge_node"`)인데, `internal/graph`도 같은 대상에 대해 not-found를 낼 때 이 상수를 재사용해서 두 패키지가 엔티티 이름을 다르게 적는 일이 없다.

### 2.2 Edge

```go
type Edge struct {
    From       string   `json:"from"`
    To         string   `json:"to"`
    Rel        Rel      `json:"rel"`
    Provenance []string `json:"provenance"`
    Confidence float64  `json:"confidence"`
}
```

`Rel`은 `relates_to` / `derived_from` / `supersedes` / `about` 네 개뿐이다. `Edge.Validate()`는 양 끝 ULID, 닫힌 `rel`, `confidence ∈ [0,1]`, 그리고 **자기 자신을 supersede할 수 없음**(`rel == supersedes && from == to` 금지, v1에서 계승)을 본다.

엣지 정체성은 `(from, to, rel)` 삼중항이다. hot 쪽 `upsertEdge`도, Neo4j 쪽 `MERGE`도, `diffEdges`의 신규 판정도, `graphFromPaths`의 중복 제거도 전부 이 삼중항을 키로 쓴다 — 같은 관계를 두 번 POST하면 새 엣지가 아니라 **교체**가 된다(`TestCreateEdgeIsIdempotentOnFromToRel`).

> ⚠️ **설계 문서와 차이 — 도메인 `Validate()`는 프로덕션 경로에서 호출되지 않는다.**
> `Node.Validate()` / `Edge.Validate()`의 호출부는 `internal/knowledge/knowledge_test.go`뿐이다. 실제 검증은 HTTP 경계에서 `CreateNodeRequest.validate()`와 `validateEdge()`(`internal/server/knowledge_graph.go`)가 따로 수행한다. 두 함수는 이제 `knowledge.ValidNodeKind` / `ValidTrust` / `ValidState` / `ValidRel`을 호출하므로 **열거값 사본은 사라졌지만**, 검사 항목 자체는 아직 완전히 겹치지 않는다 — `validateEdge`에 self-supersede 검사가 없어서 **`from == to`인 `supersedes` 엣지는 여전히 `POST .../knowledge/edges`로 만들어진다.** 재수화는 이 엣지를 그대로 Neo4j로 재생하고, Neo4j는 자기 루프를 만든다(가변 길이 경로가 같은 관계를 재사용하지 않으므로 `SupersedeChain`이 무한히 돌지는 않는다). `knowledge.go`의 주석이 이 이중화를 "지울 데드코드가 아니라 핸들러 쪽으로 합칠 대상"으로 기록해 두었고, 추적 위치는 [09 · 코드 구조](09-code-structure.md) §6.1이다.

## 3. supersede — 비파괴 개정

`knowledge.Supersede(g, newNode, supersedes, now) (Graph, error)`는 순수 함수다. 입력 그래프를 절대 변형하지 않고 새 `Graph`를 만든다. 구현은 `internal/knowledge/lifecycle.go`에 있다.

동작 순서:

1. `newNode.ID`가 이미 그래프에 있으면 `errs.Conflict`("node already exists")
2. `supersedes`를 `dedupe`. 각 대상에 대해 자기 참조면 `errs.Invalid`, 그래프에 없으면 `errs.NotFound`
3. 패자(대상) 각각: `superseded_by = newNode.ID`, **`active`였을 때만** `archived`로 이동(`deprecated`는 그대로 둔다), `updated = now`
4. 승자: `State`를 `active`로 강제, `SupersededBy = ""`, `Supersedes = targets`, `Created`가 zero면 `now`, `Updated = now`
5. 대상마다 엣지 하나: `Edge{From: winner, To: target, Rel: supersedes, Provenance: 승자 provenance 복사본, Confidence: supersedeConfidence(=1.0)}` — `hasEdge`로 이미 있으면 건너뛴다

세 에러의 Kind가 그대로 HTTP status가 된다(`apierr.From`): 409 / 400 / 404. 실제로 `POST .../knowledge/nodes`가 새 ULID를 직접 발급하므로 1번(중복 id → 409)과 2번의 자기 참조(→ 400)는 HTTP로는 도달 불가능하고, 도달하는 것은 "없는 노드를 supersede했다" → **404**뿐이다(`TestCreateNodeSupersedeMissingTargetIs404`).

`supersedes`가 비어 있으면 3~5를 건너뛰고 노드만 append한다. 승자·패자 정보가 **노드 속성(`supersedes` / `superseded_by`)과 엣지 양쪽에 중복 기록**되는데, 속성은 hot JSON만 읽어도 체인을 복원할 수 있게 하고 엣지는 Cypher 순회를 가능하게 한다. 둘은 `Supersede` 안에서 함께 만들어지므로 갈라질 수 없다.

### 3.1 상태 전이

`knowledge.Transition(n, target, reason, now)`가 v1 상태기계(`src/lifecycle.ts` 포팅)를 강제한다. 허용 표는 패키지 레벨 맵이 아니라 `allowedTargets(from State) []State` 스위치다.

| from | 허용되는 to |
|---|---|
| `active` | `archived`, `deprecated` |
| `archived` | `active`, `deprecated` |
| `deprecated` | `active` |

- 같은 상태로의 전이는 no-op으로 허용된다(단 `Updated`는 갱신)
- `deprecated`로 갈 때는 `reason`이 필수 — 비면 `errs.Conflict`("deprecating requires a reason — why is it wrong?")
- `active`로 되살릴 때 `superseded_by`를 지운다. §2.1 불변식을 자동으로 만족시키기 위한 것
- 입력 노드는 변형되지 않는다(`out := n` 값 복사)

거부는 **전부 `KindConflict`** 다 — 알 수 없는 target도, 불법 전이도, reason 누락도. "움직임을 거절하는 것은 상태기계 위반"이라는 한 가지 의미로 통일되어 있고, `apierr`가 이를 **409**로 옮긴다(`TestPatchNodeIllegalTransitionIs409`).

HTTP에서는 `PATCH .../knowledge/nodes/{id}`가 `op: "set_state" | "deprecate"`로 이 함수를 부른다. 전송 계층의 `PatchNodeRequest.targetState()`가 먼저 알 수 없는 `op`, 닫힌 집합 밖의 `state`, 빈 `reason`을 **400**으로 걸러내므로, 같은 규칙에 대한 `Transition`의 409는 방어선 두 겹째다(공백만 든 `reason`은 전송 계층을 통과하고 `Transition`의 `TrimSpace`에 걸려 409가 된다).

### 3.2 purge — archived/deprecated 버퍼가 게이트다

`knowledge.CanPurge(s)`는 `archived` 또는 `deprecated`일 때만 참이다 — v1의 "삭제 전 버퍼" 규칙. 유일한 호출자는 같은 파일의 `knowledge.Purge`다.

```go
func Purge(g Graph, id string) (Graph, int, error)
```

`Purge`가 하는 일 세 가지:

1. **존재 확인** — 없으면 `errs.NotFound` → **404**
2. **상태 게이트** — `CanPurge`가 거짓이면 `errs.Conflict`(`"purge requires state archived or deprecated — archive or deprecate the node first"`, `state` 필드 첨부) → **409**. 즉 **`active` 노드는 바로 purge되지 않는다**
3. **참조 복구** — 노드와 인접 엣지를 지우면서, 사라진 id를 가리키던 `superseded_by`는 `""`로, `supersedes` 배열에서는 해당 id를 뺀다(`withoutID`). 이걸 안 하면 개정 체인이 끊긴 채 남고, 멀쩡한 노드가 `Node.Validate`를 통과 못 하게 될 수도 있다

`handlePurgeNode`는 `confirm=true`(먼저) → ULID 형식 → `UpdateKnowledge` 클로저 안에서 `knowledge.Purge` 순서로 진행한다. 조회·게이트·제거가 한 클로저 안에 있으므로, 동시 PATCH가 바꿔놓은 상태를 상대로 게이트를 통과하는 일이 없다. 고정 테스트: `TestPurgeRequiresBuffer`(도메인), `TestPurgeNodeRefusesUnbufferedNode` / `TestPurgeNodeSucceedsAfterDeprecation` / `TestPurgeNodeRepairsDanglingRevisionLinks`(핸들러).

purge 응답은 `PurgeNodeResponse{purged_id, removed_edges, degraded}`로 **제거한 인접 엣지 수를 정직하게 보고**한다. Neo4j 쪽은 `DeleteNode`(`DETACH DELETE`)로 best-effort 반영하고, 실패하면 manifest를 dirty로 찍고 `degraded: ["graph unavailable"]`을 붙인다. dirty로 남은 삭제는 다음 부분 재수화의 `DeleteMissing`이 수렴시킨다(§5.6).

## 4. Neo4j 스키마 — 라벨 하나, 관계 타입 하나

```go
const (
    nodeLabel     = "KnowledgeNode"
    relType       = "REL"
    fulltextIndex = "knowledgeFulltext"
    searchLimit   = 50
)
```

모든 knowledge 노드가 라벨 하나(`:KnowledgeNode`)를 쓰고, 프로젝트 구분은 라벨이 아니라 **`ws` / `team` / `proj` 속성**으로 한다. 모든 엣지는 관계 타입 하나(`:REL`)를 쓰고 §2.2의 `rel`은 속성으로 들어간다. 이유는 `MERGE`다 — 관계 타입을 문자열로 조립하면 파라미터화가 불가능해지고, `MERGE (a)-[r:REL {rel: e.rel}]->(b)`로 두면 `(from, to, rel)` 삼중항 전체를 파라미터로 매칭할 수 있다.

### 4.1 속성 매핑 (`convert.go`)

`nodeProps`가 `knowledge.Node`를 Cypher 파라미터로 평탄화한다. 도메인 필드와 1:1이 아닌 부분만:

| 저장 속성 | 값 | 이유 |
|---|---|---|
| `ws` / `team` / `proj` | `hotstore.ProjectKey`의 세 조각 | 노드에 프로젝트 스코프를 직접 박는다 |
| `aliases` | 원본 리스트 (`toAnySlice`) | Neighborhood의 `IN coalesce(c.aliases, [])` 매칭용 |
| `aliases_text` | `strings.Join(aliases, " ")` | **Neo4j fulltext 인덱스는 문자열 속성만 색인한다** — 리스트는 색인되지 않아서 join한 사본을 따로 둔다 |
| `created` / `updated` | `time.RFC3339Nano` UTC 문자열, zero면 `""` (`formatTime`) | code-standards §3 "저장·비교는 RFC3339 UTC 문자열" |

`propsToNode`가 정확한 역변환이며, 없거나 타입이 다른 속성은 zero value로 떨어진다(`asString` / `asFloat` / `asStringSlice` / `parseTime`). `asStringSlice`는 드라이버가 `[]any`로 주든 `[]string`으로 주든 받아내고, 속성이 아예 없거나(`nil`) 타입이 다르면 nil 대신 빈 슬라이스를 돌려준다(`TestAsStringSlice`). 왕복은 `TestNodePropsRoundTrip` / `TestEdgePropsRoundTrip`이 고정한다.

### 4.2 인덱스 생성 (`ensureSchema`)

```cypher
CREATE INDEX knowledge_node_id IF NOT EXISTS FOR (n:KnowledgeNode) ON (n.id)
CREATE FULLTEXT INDEX knowledgeFulltext IF NOT EXISTS FOR (n:KnowledgeNode) ON EACH [n.name, n.body, n.aliases_text]
```

- `graph.Client` 인터페이스에 `EnsureIndex` 훅이 없기 때문에 스키마는 **지연 수렴**한다: `schemaMu`로 보호되는 `schemaReady` 플래그가 false인 동안, `UpsertNodes`와 `Search`가 매 호출마다 위 두 문장을 재시도한다. 한 번 성공하면 프로세스 수명 동안 다시 안 돈다(뮤텍스가 지키는 대상이 무엇인지 구조체 주석에 명시되어 있다 — code-standards §3)
- `IF NOT EXISTS`라서 멱등. 컨테이너가 새로 떠도 첫 upsert가 스키마를 다시 만든다
- `UpsertEdges` / `Neighborhood` / `SupersedeChain` / `NodeCount` / `DeleteNode` / `DeleteMissing` / `Clear`는 `ensureSchema`를 부르지 않는다 — 인덱스 없이도 정확한 답이 나오는 질의들이다

## 5. Cypher 패턴

읽고 쓰는 질의는 전부 `internal/graph/queries.go`에 모여 있고, 세션·트랜잭션 실행은 `bolt.go`의 `boltRunner`가 맡는다. 프로젝트 스코프는 대부분 `withKey`가 주입하는 `$ws` / `$team` / `$proj` 파라미터로 좁힌다 — `UpsertEdges` · `DeleteNode` · `DeleteMissing` · `Search` · `Neighborhood` · `SupersedeChain`, 그리고 키가 zero value가 아닐 때의 `NodeCount`가 여기 해당한다. 예외 셋: `UpsertNodes`는 `$ws`를 쓰지 않고 `UNWIND`된 행마다 `nodeProps`가 박아 둔 `n.ws` / `n.team` / `n.proj`로 MERGE 키를 잡고, `Clear`와 `ensureSchema`는 애초에 프로젝트 개념이 없는 전역 질의다. 세션은 질의마다 새로 열고, 읽기는 `ExecuteRead`(`c.read`), 쓰기는 `ExecuteWrite`(`c.write`)로 분리한다.

### 5.1 노드 upsert — 멱등이 필수인 이유

```cypher
UNWIND $nodes AS n
MERGE (k:KnowledgeNode {id: n.id, ws: n.ws, team: n.team, proj: n.proj})
SET k.kind = n.kind, k.name = n.name, k.body = n.body,
    k.aliases = n.aliases, k.aliases_text = n.aliases_text,
    k.state = n.state, k.trust = n.trust,
    k.supersedes = n.supersedes, k.superseded_by = n.superseded_by,
    k.provenance = n.provenance,
    k.created = n.created, k.updated = n.updated, k.review_after = n.review_after
```

`MERGE` 매칭 키는 `{id, ws, team, proj}`이고 `SET`은 §2.2의 **모든 속성을 무조건 덮어쓴다**. 이 두 성질이 합쳐져야 재수화가 성립한다:

- Neo4j 컨테이너는 볼륨 없이 뜬다(§8, 의도적 비영속). 죽으면 노드 0개에서 시작한다
- 복구는 hot JSON을 그대로 다시 재생하는 것이다 — `rehydrate`의 `rehydrateKnowledgeAll`이 프로젝트마다 `ReadKnowledge` → `UpsertNodes` → `UpsertEdges`를 돌린다
- 이 재생은 **부분 실패 후에도, 중복 실행돼도 같은 상태로 수렴해야 한다**. 그래서 create가 아니라 MERGE, 부분 SET이 아니라 전체 SET이다. 살아 있는 노드에 재생을 덮어쓰면 hot이 이긴다 — 파생물에만 있던 값은 사라지는 게 정답이다(§0 원칙 1)
- 다만 MERGE만으로는 **사라진 노드**를 지울 수 없다. 전체 재수화는 앞에서 `Clear`로, 프로젝트 단위 재수화는 뒤에서 `DeleteMissing`으로 그 구멍을 막는다(§5.6)
- 라이브 테스트 `TestLiveUpsertIdempotentAndCount`가 같은 노드/엣지를 두 번 upsert한 뒤 `NodeCount == 2`를 확인해 이 성질을 고정한다

자세한 재수화 흐름은 [07 · 재수화](07-rehydration.md).

### 5.2 엣지 upsert — 끝점이 없으면 조용히 건너뛰되, 보고한다

```cypher
UNWIND $edges AS e
MATCH (a:KnowledgeNode {id: e.from, ws: $ws, team: $team, proj: $proj})
MATCH (b:KnowledgeNode {id: e.to, ws: $ws, team: $team, proj: $proj})
MERGE (a)-[r:REL {rel: e.rel}]->(b)
SET r.provenance = e.provenance, r.confidence = e.confidence
RETURN count(r) AS merged
```

`MATCH`가 실패한 행은 결과에서 빠지므로 끝점이 없는 엣지는 만들어지지 않는다(고아 엣지 방지). 대신 `merged < len(edges)`면 `c.log.WarnContext(ctx, "graph edges skipped (endpoint missing)", "project", …, "requested", …, "merged", …)`로 요청 수/반영 수를 함께 남긴다. 로거는 `Config.Logger`로 주입되고(없으면 `slog.Default()`), 전역 `slog` 호출은 없다. hot-first 쓰기 순서상 정상 경로에서는 끝점이 항상 먼저 존재하므로, 이 경고는 **순서가 뒤집힌 재생**의 신호다.

### 5.3 이웃 순회 — `GET .../knowledge/graph?entity=&depth=`

```cypher
MATCH (c:KnowledgeNode {ws: $ws, team: $team, proj: $proj})
WHERE c.name = $entity OR $entity IN coalesce(c.aliases, [])
WITH c LIMIT 1
OPTIONAL MATCH p = (c)-[:REL*1..N]-(m:KnowledgeNode)   // N = clampDepth(depth) ∈ [1,10]
WHERE all(x IN nodes(p) WHERE x.ws = $ws AND x.team = $team AND x.proj = $proj)
RETURN c, collect(p) AS paths
```

- **depth만 문자열 보간이다.** Cypher는 가변 길이 경계를 파라미터로 못 받는다. 그래서 `clampDepth(depth)`가 `[1, maxDepth=10]`으로 좁힌 **정수**를 `fmt.Sprintf`로 끼워 넣는다(주입 여지 없음. 함께 보간되는 `nodeLabel` / `relType`도 패키지 상수다). HTTP 계층에서도 `parseDepthParam`이 `defaultGraphDepth=1` / `maxGraphDepth=10` 밖의 값을 400으로 먼저 잘라낸다
- 순회는 **방향 무시**다(`-[:REL*1..N]-`). "이 entity에 붙은 것 전부"가 질문이므로 엣지 방향은 필터가 아니다
- `all(x IN nodes(p) ...)` 술어가 경로 **전체**를 프로젝트 안에 가둔다. 중간 노드 하나라도 다른 프로젝트면 그 경로는 통째로 탈락
- `WITH c LIMIT 1` — 중심은 하나만 고른다. 같은 `name`이나 alias를 가진 노드가 둘이면 **어느 쪽이 뽑힐지는 정해져 있지 않다**(스캔 순서 의존)
- 없는 entity는 `errs.NotFound(op, knowledge.EntityNode, entity)` → **404**

`graphFromPaths`가 경로 묶음을 `knowledge.Graph`로 접는다. 관계의 끝점은 드라이버에서 element id로 오기 때문에, **먼저 모든 경로 노드를 훑어 element id → ULID 맵을 채운 뒤에** 엣지를 변환한다. 노드는 ULID로, 엣지는 `(from, to, rel)` 시그니처로 중복 제거하며, 중심 노드가 항상 `Nodes[0]`이다.

### 5.4 supersede 체인 walk

```cypher
MATCH (n:KnowledgeNode {id: $id, ws: $ws, team: $team, proj: $proj})
OPTIONAL MATCH (n)-[:REL*1..50 {rel: 'supersedes'}]->(older:KnowledgeNode)
WITH n, collect(DISTINCT older) AS olders
OPTIONAL MATCH (newer:KnowledgeNode)-[:REL*1..50 {rel: 'supersedes'}]->(n)
RETURN n, olders, collect(DISTINCT newer) AS newers
```

- **양방향**이다. 시작 노드가 대체한 것(`olders`, 나가는 방향)과 시작 노드를 대체한 것(`newers`, 들어오는 방향)을 각각 모은다. 체인 중간 노드에서 물어봐도 전체가 나온다
- `{rel: 'supersedes'}` 술어는 가변 길이 경로의 **모든 관계**에 적용되므로 `about`이나 `relates_to`로 새지 않는다. 리터럴은 `knowledge.RelSupersedes` 상수를 보간한 것이라 도메인 어휘와 갈라질 수 없다
- `maxChainHops = 50` — 순환 import가 질의를 잡아먹지 못하게 막는 홉 상한
- 결과는 `sortChainOldestFirst`가 `Created` 오름차순, 동률이면 ULID로 타이브레이크해 **오래된 것부터** 돌려준다. ULID가 시간 정렬 가능하다는 성질이 여기서 결정성을 만든다
- 없는 id는 `errs.NotFound`

> **HTTP 표면 없음**: `SupersedeChain`은 `graph.Client` 인터페이스에 있지만 라우터에 연결된 엔드포인트가 없고, 서버가 선언한 narrow interface `server.KnowledgeGraph`에도 **일부러 빠져 있다**(핸들러가 안 쓰는 메서드는 넣지 않는다 — code-standards §1.1). 존재 근거는 설계 문서 §10.3의 수용 기준 "Neo4j 순회로 체인 확인"이고, 현재 호출자는 `internal/graph/client_test.go` · `live_test.go`와 블랙박스 시나리오 3·5(`TestScenario03_KnowledgeSupersedeChain`, [11 · 검증](11-testing.md))다. §7의 knowledge 엔드포인트 목록에도 체인 조회는 없으므로 스펙과는 일치하지만, 에이전트가 체인을 보려면 `GET .../knowledge/graph`로 이웃을 받아 `supersedes` 엣지를 직접 따라가야 한다.

### 5.5 fulltext 검색 — `GET .../knowledge/search?q=&include_archived=`

```cypher
CALL db.index.fulltext.queryNodes($index, $q) YIELD node, score
WHERE node.ws = $ws AND node.team = $team AND node.proj = $proj
  AND ($includeArchived OR node.state = 'active')
RETURN node ORDER BY score DESC LIMIT 50
```

- 인덱스는 `name`, `body`, `aliases_text` 세 속성을 덮는다
- **인덱스 자체는 프로젝트 스코프가 아니다.** 전체 인덱스에서 히트를 받아 `WHERE`로 프로젝트를 거르고, `LIMIT 50`(`searchLimit`)은 거른 **뒤에** 걸린다. 정확도는 보장되지만 다른 프로젝트 노드가 많을수록 필터링 비용이 는다
- **archived/deprecated는 기본 제외**, `include_archived=true`일 때만 포함(§7 옵트인). 라이브 테스트 `TestLiveFulltextSearchAndArchivedOptIn`이 이 두 갈래를 모두 고정한다
- `$q`는 파라미터로 전달되지만 내용은 **Lucene 문법**이다. 문법이 깨진 질의(`AND`로 끝나거나 따옴표가 안 맞는 등)는 연결 실패가 아니므로 `mapErr`가 `KindUnavailable`이 아닌 `KindInternal`로 분류하고, 결과적으로 **500**이 된다
- 형태소 분석이 없다. 인덱스를 옵션 없이 만들었으므로 Neo4j 기본 분석기가 쓰이고, episodic 쪽 nori 형태소 검색([04 · Episodic 검색](04-episodic-search.md))과 달리 **한국어는 토큰 일치 수준**으로만 잡힌다. 같은 "검색"이라도 두 평면의 재현율 특성이 다르다는 뜻이다
- fulltext 색인은 비동기로 채워진다. 라이브 테스트가 최대 15초간 폴링하는 이유이며, 쓰기 직후 검색이 비는 순간이 존재한다

### 5.6 나머지

| 메서드 | Cypher | 쓰임 |
|---|---|---|
| `DeleteNode` | `MATCH (n:KnowledgeNode {id, ws, team, proj}) DETACH DELETE n` | purge 경로 전용(§3.2) |
| `DeleteMissing` | `MATCH (n:KnowledgeNode {ws, team, proj}) WHERE NOT n.id IN $keep DETACH DELETE n` | **프로젝트 단위** 재수화의 제거 수렴. MERGE는 추가/갱신만 하므로, 라이브 삭제가 실패한 purge가 재생마다 되살아나는 것을 막는다. `keep`이 nil이면 `[]`로 바꿔 보낸다 — Cypher `null`에 대한 `IN`은 절대 false가 아니라서 전량 삭제가 안 되기 때문 |
| `NodeCount` | `MATCH (n:KnowledgeNode {…}) RETURN count(n)` — zero-value 키면 스코프 없이 전체 | manifest 드리프트 대조(§5), 재수화 `verify` |
| `Clear` | `MATCH (n:KnowledgeNode) DETACH DELETE n` | **전체** 재수화·재해 복구 reindex. 인덱스는 살아남고 노드만 지운다. 프로젝트 단위 경로는 다른 프로젝트를 건드리면 안 되므로 이걸 쓰지 않는다 |
| `Ping` | `boltRunner.Verify` → `driver.VerifyConnectivity` | 기동 시·드리프트 판정 시 도달성 |

## 6. 쓰기 경로 — hot이 먼저, 그래프는 best-effort

`POST .../knowledge/nodes`(`handleCreateNode`)의 순서는 고정되어 있다.

1. `validateProjectKey` → `CreateNodeRequest.validate()` (kind/name/trust/provenance ULID/supersedes ULID/review_after)
2. `now := s.clock.Now().UTC()` → `s.ids.GenerateAt(now.UnixMilli())`로 id 부여, `state: active`, `aliases`·`provenance`는 `normalizeStrings`(trim + 빈 문자열 제거, **절대 nil이 아님** — hot JSON에 `null` 대신 `[]`가 들어가도록). 시계와 id 생성기는 둘 다 주입된 의존(`server.Clock` / `server.IDGenerator`)이라 `time.Now()` 직접 호출이 없고, id의 시간 절반이 항상 주입된 시계와 일치한다
3. `UpdateKnowledge` 클로저 안에서 현재 그래프를 받아 — `supersedes`가 있으면 `knowledge.Supersede`, 없으면 노드만 append — 새 그래프와 함께 `affectedNodes` / `diffEdges`를 계산한다. **여기가 커밋 지점.** 읽기·결정·쓰기가 전부 스토어 락 안에서 일어나므로 동시 쓰기가 서로를 덮어쓰지 못한다(`TestConcurrentCreateNodeKeepsEveryNode`). 실패하면 `apierr.From`이 도메인 Kind대로 400/404/409/500을 내고 그래프는 건드리지 않는다
4. `mirrorKnowledge`: `affectedNodes`(새 노드 + 전이된 패자들만) + `diffEdges`(직전 그래프에 없던 엣지만)를 골라 최소 범위로 `UpsertNodes` → `UpsertEdges`
5. `promoteProvenance`: `provenance`가 가리키는 hot episode들을 `consolidated=true`로 찍고 검색 인덱스를 맞춘다([03 · 생명주기](03-lifecycle.md))

4·5가 실패해도 요청은 여전히 **201**이다. 4가 실패하면 `data.degraded: ["graph unavailable"]`와 함께 manifest에 `PlaneKnowledge` dirty가 찍히고, 5가 실패하면 `"provenance episodes not marked consolidated"`가 붙는다. 성공하면 `markIndexed`. degraded는 에러가 아니라는 규약(code-standards §2.2)이 여기 그대로 적용된다. `PATCH`(200)와 `POST .../knowledge/edges`(201)도 같은 규약을 따른다.

반대로 **읽기 경로**(`/knowledge/search`, `/knowledge/graph`)는 `s.graph == nil`이거나 에러가 `errors.Is(err, errs.ErrUnavailable)`면 **503**을 낸다 — 파생물이 없으면 대답할 수 없기 때문이다. 503 본문의 문구는 degraded 노트와 **글자 그대로 같은 `"graph unavailable"`** 이다(`internal/server/degraded.go`의 `degradedGraph` 상수 하나를 양쪽이 공유).

`KindUnavailable` 판정 기준은 `bolt.go`의 `mapErr` 한 곳이다: `neo4j.IsConnectivityError(err)` 또는 `errors.Is(err, context.DeadlineExceeded)`일 때만 `errs.Unavailable`로 감싸고, 이미 Kind를 가진 에러는 Kind를 유지한 채 op만 덧붙이며(`errs.Wrap`), 나머지 질의 에러는 `errs.Internal`이 된다. 예외는 `Ping` 하나 — 도달성 확인은 실패 사유를 가리지 않고 전부 `errs.Unavailable`이다. 그게 degraded 판정에 필요한 신호 자체이기 때문이다.

## 7. provenance — knowledge에서 episodic으로 되짚기

`Node.Provenance`와 `Edge.Provenance`는 **유래 episode의 ULID 배열**이다. 서버는 이 값을 만들지 않는다 — 증류(episode 묶음 → 사실)는 에이전트의 일이고(§0 원칙 2), 서버는 `validateULIDs`로 형식만 확인한 뒤 그대로 싣는다.

이 링크가 깨지지 않는 이유는 episode id가 불변이기 때문이다. episode는 통합 후 30일이 지나면 hot에서 사라지고 S3 `{yyyy-mm}.json`으로 내려가지만([03 · 생명주기](03-lifecycle.md)), id는 그대로라 아카이브에서 다시 찾을 수 있다. 다이어그램의 회색 episode가 그 경우다.

구현상 알아둘 점:

- **provenance는 그래프 엣지가 아니다.** Neo4j에는 episode 노드가 없다. provenance는 노드/관계의 **리스트 속성**으로만 저장되고, 이를 따라가는 Cypher 질의도 없다. 되짚기는 호출자가 id로 `GET .../episodes/{id}`를 치는 방식이며, hot에 없으면 그 핸들러가 cold 아카이브까지 찾아본다
- `Supersede`는 승자의 provenance를 **복사해서** supersedes 엣지에 심는다(`slices.Clone(winner.Provenance)`) — 슬라이스 공유로 인한 원격 변형이 없다
- provenance는 단순 기록이 아니라 **통합 승격의 선언**이기도 하다. `promoteProvenance`가 그 id들의 hot episode를 `consolidated=true`로 찍고, 그 플래그가 §3.1 에이징의 전제조건이 된다. 서버가 판단하는 것은 없다 — 에이전트가 "이 episode들을 증류했다"고 말한 결과를 기록할 뿐이다
- 문서 노드는 예외적으로 서버가 만든다. `document` 패키지의 `ensureDocumentNode`가 `kind: document`, `name: 파일명`, `body: "sha256:…"`, `aliases: [sha]`, `trust: imported`, `provenance: []`인 노드를 자동 생성한다. **엣지는 하나도 만들지 않는다** — 문서에서 배운 사실을 `derived_from`으로 잇는 건 에이전트 몫이라는 §6 5번 규정 그대로다([06 · 문서](06-documents.md))
- 같은 sha를 다시 올리면 `aliases`에 sha가 든 기존 document 노드를 재사용한다(멱등). 조회와 append가 한 `UpdateKnowledge` 클로저 안에 있어서 동시 ingest가 서로의 노드를 덮어쓰지 않는다

## 8. trust / state 가중 — 실제로 가중되는 것은 state뿐

세 개의 "신뢰" 신호가 스키마에 있다. 코드가 실제로 무엇을 하는지는 다르다.

| 신호 | 저장 | 검증 | 질의에 미치는 영향 |
|---|---|---|---|
| `state` | 노드 속성 | 닫힌 집합 + active 불변식 + 상태기계 + purge 게이트 | **있음.** fulltext 검색이 `node.state = 'active'`로 거르고, `include_archived=true`일 때만 archived/deprecated가 나온다. purge 가능 여부도 state가 정한다 |
| `trust` | 노드 속성 | 닫힌 집합 | **없음.** 순위·가중·충돌 판정 어디에도 쓰이지 않는다. 저장되고 왕복될 뿐 |
| `confidence` | 엣지 속성 | `[0,1]` | **없음.** 순회 순서나 필터에 쓰이지 않는다. `Supersede`가 만드는 엣지는 `supersedeConfidence = 1.0` 고정 |

정렬 기준은 fulltext의 Lucene `score DESC` 하나뿐이고, 이웃 순회에는 순위 개념 자체가 없다. 이건 누락이 아니라 §0 원칙 2의 결과다 — **어떤 사실을 믿을지는 서버가 정하지 않는다.** 서버는 `trust`, `confidence`, `state`, `supersedes` 체인을 전부 응답에 실어 보내고, 가중은 그걸 읽는 에이전트가 한다. 서버가 자동으로 하는 유일한 "판정"은 supersede 요청을 받았을 때 패자를 archived 버퍼로 옮기는 것뿐이며, 그 요청조차 에이전트가 명시적으로 보낸 것이다.

## 9. 코드 위치

| 개념 | 파일 | 심볼 |
|---|---|---|
| 노드·엣지 도메인 타입, 닫힌 값 집합 | `internal/knowledge/knowledge.go` | `Node`, `Edge`, `Graph`, `NodeKind`, `State`, `Trust`, `Rel`, `ValidNodeKind`, `ValidState`, `ValidTrust`, `ValidRel` |
| 불변식 검증, 에러 엔티티 이름 | `internal/knowledge/knowledge.go` | `Node.Validate`, `Edge.Validate`, `EntityNode`, `EntityEdge` |
| 상태기계 / purge 게이트 / 비파괴 개정 | `internal/knowledge/lifecycle.go` | `allowedTargets`, `Transition`, `CanPurge`, `Purge`, `withoutID`, `Supersede`, `FindNode`, `hasEdge`, `dedupe` |
| 도메인 에러 어휘 (`*errs.Error`, 5종 Kind·센티넬) | `internal/errs/errs.go` | `Kind`, `ErrInvalid`/`ErrNotFound`/`ErrConflict`/`ErrUnavailable`/`ErrInternal`, `Invalid`, `NotFound`, `Conflict`, `Unavailable`, `Internal`, `Wrap` |
| Neo4j 계약·스키마 상수·지연 DDL | `internal/graph/graph.go` | `Client` 인터페이스, `Config`, `New`, `client`, `Ping`, `Close`, `ensureSchema`, `nodeLabel`, `relType`, `fulltextIndex`, `searchLimit` |
| MERGE upsert·순회·체인·검색·삭제 | `internal/graph/queries.go` | `UpsertNodes`, `UpsertEdges`, `Search`, `Neighborhood`, `SupersedeChain`, `DeleteNode`, `DeleteMissing`, `NodeCount`, `Clear` |
| 드라이버 narrow interface·세션 실행·에러 분류 | `internal/graph/bolt.go` | `runner`, `boltRunner`, `mapErr` |
| 속성 변환·깊이 상한·경로 접기 | `internal/graph/convert.go` | `nodeProps`, `edgeProps`, `propsToNode`, `clampDepth`, `maxDepth`, `maxChainHops`, `graphFromPaths`, `sortChainOldestFirst`, `withKey` |
| HTTP 핸들러 | `internal/server/handlers_knowledge.go` | `handleCreateNode`, `handleCreateEdge`, `handlePatchNode`, `handlePurgeNode`, `handleSearchKnowledge`, `handleKnowledgeGraph` |
| 요청 DTO·경계 검증·상태 전이 op | `internal/server/handlers_knowledge.go` | `CreateNodeRequest.validate`, `PatchNodeRequest.targetState`, `NodeResponse`, `EdgeResponse`, `PurgeNodeResponse` |
| 그래프 미러링·provenance 승격 | `internal/server/handlers_knowledge.go` | `mirrorKnowledge`, `promoteProvenance` |
| 순수 그래프 대수·엣지 경계 검증 | `internal/server/knowledge_graph.go` | `affectedNodes`, `diffEdges`, `findGraphNode`, `validateEdge`, `upsertEdge`, `nodeExists` |
| ULID·깊이 범위 검증 | `internal/server/validate.go` | `validateULID`, `validateULIDs`, `normalizeStrings`, `parseDepthParam`, `defaultGraphDepth`, `maxGraphDepth` |
| 소비자 측 narrow interface | `internal/server/deps.go` | `KnowledgeGraph`, `HotStore`, `Clock`, `IDGenerator` |
| degraded 어휘·manifest 마킹 | `internal/server/degraded.go` | `degradedGraph`, `degradedPromotion`, `markIndexed`, `markDirty` |
| Kind → HTTP status 단일 매핑 | `internal/server/apierr/apierr.go` | `From`, `mappingFor`, `publicMessage` |
| hot 정본 IO | `internal/hotstore/knowledge.go` (계약은 `hotstore.go`의 `Client`) | `client.ReadKnowledge`, `client.UpdateKnowledge`, `writeKnowledgeLocked`, `readKnowledgeLocked` |
| MERGE 재생·제거 수렴·드리프트 | `internal/rehydrate/hydrate.go` · `internal/rehydrate/rehydrate.go` | `rehydrateKnowledgeAll`, `rehydrateProjectKnowledge`, `knowledgeDrift`, `hotNodeCount` |
| 문서 노드 자동 생성 | `internal/document/ingest.go` | `ensureDocumentNode` |
| 도메인 단위 테스트 | `internal/knowledge/knowledge_test.go` · `lifecycle_test.go` | `TestNodeValidate`, `TestEdgeValidate`, `TestTransition`, `TestCanPurge`, `TestSupersede`, `TestPurgeRequiresBuffer`, `TestPurgeDropsEdgesAndRepairsReferences` |
| 변환 단위 테스트 | `internal/graph/convert_test.go` | `TestGraphFromPaths`, `TestSortChainOldestFirst`, `TestClampDepth`, `TestNodePropsRoundTrip` |
| fake 드라이버 클라이언트 테스트 | `internal/graph/client_test.go` | `TestUpsertNodes`, `TestUpsertEdges`, `TestNeighborhood`, `TestSupersedeChain`, `TestSearch`, `TestDeleteMissing`, `TestMapErr` |
| 실물 Neo4j 테스트 | `internal/graph/live_test.go` | `TestLiveUpsertIdempotentAndCount`, `TestLiveFulltextSearchAndArchivedOptIn`, `TestLiveSupersedeChainAndPurge`, `TestLiveUnavailableIsDegradedSignal` |
| 핸들러 테스트 (supersede·purge·degraded) | `internal/server/handlers_knowledge_test.go` | `TestCreateNodeSupersedeArchivesPredecessor`, `TestPatchNodeIllegalTransitionIs409`, `TestPurgeNodeRefusesUnbufferedNode`, `TestPurgeNodeRepairsDanglingRevisionLinks` |
| 동시성 테스트 | `internal/server/handlers_knowledge_concurrency_test.go` | `TestConcurrentCreateNodeKeepsEveryNode`, `TestConcurrentCreateEdgeKeepsEveryEdge`, `TestConcurrentPatchAndCreateKeepBothEffects` |
| 블랙박스 수용 시나리오 3 | `test/blackbox/blackbox_test.go` | `TestScenario03_KnowledgeSupersedeChain` |
