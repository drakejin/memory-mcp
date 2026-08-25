# 08 — HTTP API: go-chi 라우팅과 Swagger

루프백에만 묶인 go-chi 라우터가 16개 엔드포인트를 `{success, data, error}` 봉투로 노출하고, 실패는 `*errs.Error → apierr.From → *apierr.Error` 한 경계를 지나 `{code, message}`가 되며, swaggo 주석이 그 스펙을 `docs/swagger.json`으로 생성한다.

| 항목 | 내용 |
|---|---|
| 관련 코드 | [`internal/server/server.go`](../../internal/server/server.go) · [`deps.go`](../../internal/server/deps.go) · [`respond.go`](../../internal/server/respond.go) · [`validate.go`](../../internal/server/validate.go) · [`degraded.go`](../../internal/server/degraded.go) · [`startup.go`](../../internal/server/startup.go) · [`handlers_episodic.go`](../../internal/server/handlers_episodic.go) · [`handlers_knowledge.go`](../../internal/server/handlers_knowledge.go) · [`knowledge_graph.go`](../../internal/server/knowledge_graph.go) · [`handlers_documents.go`](../../internal/server/handlers_documents.go) · [`handlers_ops.go`](../../internal/server/handlers_ops.go) · [`apierr/apierr.go`](../../internal/server/apierr/apierr.go) · [`internal/errs/errs.go`](../../internal/errs/errs.go) · [`cmd/memory-mcp/main.go`](../../cmd/memory-mcp/main.go) · [`internal/config/config.go`](../../internal/config/config.go) |
| 관련 스펙 | [01-overview](01-overview.md) · [03-lifecycle](03-lifecycle.md) · [04-episodic-search](04-episodic-search.md) · [05-knowledge-graph](05-knowledge-graph.md) · [06-documents](06-documents.md) · [07-rehydration](07-rehydration.md) · [10-operations](10-operations.md) · [11-testing](11-testing.md) · 인덱스: [README](README.md) |
| 상태 | 구현 완료 — 라우트 16개 + `/swagger/*`, 전부 핸들러 본체 존재. `docs/swagger.json`은 라우터와 1:1 동기(오퍼레이션 16개 · definition 32개) |

![memory-mcp HTTP 라우트 트리 — chi.NewRouter 아래 /healthz, /swagger/*, /v1 서브라우터와 프로젝트 스코프 중첩 라우터](assets/08-http-api.svg)

---

## 1. 바인딩과 신뢰 경계 — 인증이 없는 이유

인증 코드가 없는 것이 아니라, **인증이 필요 없는 곳에만 뜨도록 설정 단계에서 강제**한다. 검사는 두 번 있다 — 설정 로드와 서버 생성.

```go
// internal/config/config.go
const DefaultListenAddr = "127.0.0.1:8420"

func (c Config) Validate() error {
	...
	if !isLoopback(c.ListenAddr) {
		return errs.Invalid(opValidate, entityConfig, "listen addr must bind loopback only").
			WithField("listen_addr", c.ListenAddr)
	}
	...
}

func isLoopback(addr string) bool {
	return strings.HasPrefix(addr, loopbackIPPrefix) || strings.HasPrefix(addr, loopbackHostPrefix)
}
```

```go
// internal/server/server.go — Config.validate: 같은 규칙을 서버 생성자가 한 번 더 본다
if !strings.HasPrefix(c.ListenAddr, loopbackIPPrefix) && !strings.HasPrefix(c.ListenAddr, loopbackHostPrefix) {
	return errs.Invalid(opNew, entityConf, "listen addr must bind loopback only").
		WithField("listen_addr", c.ListenAddr)
}
```

- 주소는 `DJ_MEMORY_LISTEN_ADDR`로 바꿀 수 있지만 `127.0.0.1:` 또는 `localhost:` 접두사가 아니면 `config.Load()`가 `errs.KindInvalid` 에러를 내고, `run()`이 그 에러를 `main`에 돌려주면 `main`이 로그를 남긴 뒤 `os.Exit(exitFailure)`(=1) 한다. `0.0.0.0:8420`으로 띄우는 경로 자체가 없다 — 설정을 우회해 `server.New`를 직접 부르는 코드도 같은 이유로 거절된다.
- 따라서 요청자는 항상 같은 호스트의 프로세스다. 토큰·세션·CORS·CSRF 계층이 전부 빠져 있고, 그 대신 파괴적 연산은 **명시적 확인 파라미터**로 막는다(`DELETE …?confirm=true`).
- 라우터에는 미들웨어가 하나도 없다. `chi.NewRouter()` 직후 바로 라우트를 붙인다 — 로깅·RequestID·Recoverer·CORS·rate limit 모두 없음. 관측은 핸들러 안의 `s.log` 호출과 실패 응답 경로(`writeAPIError`)의 단일 로그 라인으로만 한다.

```go
// internal/server/server.go — ListenAndServe
httpServer := &http.Server{
	Addr:              s.listenAddr,
	Handler:           s.Router(),
	ReadHeaderTimeout: readHeaderTimeout,
}
```

`readHeaderTimeout = 5 * time.Second`, `shutdownGrace = 10 * time.Second`. `ctx` 취소(SIGINT/SIGTERM) 시 10초 grace로 `Shutdown`. 요청 단위 타임아웃은 없다 — 대용량 문서 인제스트가 초 단위를 넘길 수 있기 때문이다.

> `Router()`가 반환하는 것은 `http.Handler`이므로, 단위 테스트는 `newTestServer`가 만든 `srv.Router()`에 `httptest.NewRecorder()`로 요청을 직접 먹여 같은 트리를 검증하고, 블랙박스는 `go build ./cmd/memory-mcp`로 만든 실제 바이너리를 띄운다([11-testing](11-testing.md)).

---

## 2. 응답 봉투 `{success, data, error}`

```go
// internal/server/respond.go
type Envelope struct {
	Success bool          `json:"success"`
	Data    any           `json:"data"`
	Error   *apierr.Error `json:"error,omitempty"`
}
```

`error`는 문자열이 아니라 **객체**다. 클라이언트는 자유 문장 대신 안정적인 `{code, message}` 쌍을 본다.

```go
// internal/server/apierr/apierr.go — 직렬화되는 것은 code/message/details 뿐이다
type Error struct {
	Status  int            `json:"-"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	cause   error          // 로깅 전용 — 응답에 나가지 않는다
}
```

핸들러가 직접 `json.NewEncoder`를 만지는 곳은 없다. 전부 두 함수만 쓴다.

| 함수 | 출력 | 비고 |
|---|---|---|
| `writeJSON(w, log, status, data)` | `{"success":true,"data":…}` | `error`는 `omitempty`로 생략 |
| `writeAPIError(w, log, apiErr)` | `{"success":false,"data":null,"error":{"code":…,"message":…}}` | `Data`에 `omitempty`가 없어 **에러 응답에는 항상 `"data":null`이 들어간다** |

`Content-Type: application/json`은 두 함수가 직접 세팅한다. `writeAPIError`는 **요청 실패를 로그로 남기는 유일한 지점**이다 — `apiErr.Unwrap()`에 원인이 있을 때만 `log.Error("request failed", "status", …, "code", …, "cause", …)` 한 줄을 남기고(원인 없는 순수 전송 계층 거절은 로그도 없다), 본문에는 `Message`만 나간다. 그래서 OpenSearch 응답 본문 같은 내부 문자열이 클라이언트로 새지 않는다 — `validate_test.go`의 `TestEnvelopeShape` "cause stays out of the body" 케이스가 이걸 고정한다. `apiErr`가 nil로 들어와도 패닉 대신 500 `internal`을 답하고, `log`가 nil이면 `slog.Default()`로 떨어진다.

**봉투가 아닌 두 경우가 있다.**

1. `GET /v1/documents/{sha}` 성공 응답은 `application/octet-stream` 원본 바이트 스트림이다(실패 시에만 봉투).
2. 라우터에 등록되지 않은 경로/메서드는 chi 기본 핸들러가 처리한다. 커스텀 `NotFound`/`MethodNotAllowed`를 붙이지 않았고, **chi가 주는 둘의 모양은 서로 다르다** — 404는 `http.NotFound`라서 `Content-Type: text/plain; charset=utf-8`에 `404 page not found` 본문이 있지만, 405는 chi 자체 핸들러(`mux.go` `methodNotAllowedHandler`)라서 `Allow` 헤더만 붙고 **본문이 비어 있다**(`Content-Length: 0`, `Content-Type` 없음). `validate_test.go`의 `TestUnknownRouteIs404`가 앞쪽을 고정한다.

### 2.1 두 계층 에러 — `*errs.Error` → `apierr.From` → `*apierr.Error`

핸들러 아래 모든 계층(`hotstore`·`search`·`graph`·`cold`·`blob`·`document`·`knowledge`·`consolidate`·`rehydrate`)은 HTTP를 모르고 `*errs.Error`만 돌려준다. 상태 코드가 정해지는 곳은 `apierr.From` 단 하나이며, 서버 패키지 안에서 이 경계를 통과하는 호출은 **18곳**이다(episodic 5 · knowledge 7 · documents 3 · ops 3 — `grep -c 'apierr\.From(' internal/server/handlers_*.go`, 테스트 제외). 18곳 전부 `writeAPIError(w, s.log, apierr.From(err))` 한 줄 형태다.

| 도메인 `errs.Kind` | HTTP | `apierr` code |
|---|---|---|
| `KindInvalid` | 400 | `invalid_request` |
| `KindNotFound` | 404 | `not_found` |
| `KindConflict` | 409 | `conflict` |
| `KindUnavailable` | 503 | `unavailable` |
| `KindInternal`·미분류 | 500 | `internal` |

- 공개 문구는 `publicMessage`가 고른다: 체인을 바깥에서 안으로 훑어 **처음 만나는 authored `Msg`**를 쓴다. `errs.Wrap`은 자기 `Msg`를 만들지 않으므로 실질적으로 실패를 인지한 가장 안쪽 지점의 문구가 그대로 올라오고(`apierr_test.go`의 "errs.Wrap keeps the innermost public message"), 순수 래핑만 있으면 Kind 기본 문구(`not found`, `service unavailable`, `internal error` …)로 떨어진다.
- 전송 계층 고유의 실패 — 망가진 body, 도메인까지 가지도 못한 경로/쿼리 파라미터, 아직 만들어지지 못한 협력자 — 만 `respond.go`의 세 생성자로 직접 만든다.

| 생성자 | 상태 | code | 쓰임 |
|---|---|---|---|
| `badRequest(msg)` | 400 | `invalid_request` | 경로·쿼리·body 검증 실패 |
| `notFound(msg)` | 404 | `not_found` | 전송 계층이 스스로 판정한 부재 — 호출은 3곳: hot 미스 + archiver 미구성, 엣지 끝점 노드 없음, PATCH 대상 노드 없음 |
| `unavailable(msg)` | 503 | `unavailable` | 협력자가 nil이거나(hot store·문서 파이프라인·consolidator·rehydrator 포함) 파생 저장소가 `KindUnavailable`을 답한 경우 |

뒤의 두 생성자는 `UpdateKnowledge` 클로저 안에서 반환돼도 안전하다 — `apierr.From`이 이미 전송 에러인 값을 그대로 통과시키기 때문에(`errors.As(err, &transport)`), `errs.Wrap`으로 감싸여 올라와도 404가 500으로 바뀌지 않는다.

`unavailable`만 `apierr.From`을 거치지 않는 이유는 §5의 degraded 어휘를 **글자 그대로** 재사용하기 위해서다. 쓰기가 실었을 degraded 노트와 읽기가 답하는 503 본문이 같은 문자열이다. 원인이 있으면 `.WithCause(err)`로 붙여 로그에만 남긴다.

---

## 3. 라우트 표면 — `Router()` 등록 순서 그대로

```go
r.Get("/healthz", s.handleHealthz)
r.Get("/swagger/*", httpSwagger.WrapHandler)

r.Route("/v1", func(r chi.Router) {
	r.Get("/status", s.handleStatus)
	r.Post("/consolidate", s.handleConsolidate)
	r.Post("/reindex", s.handleReindex)

	r.Get("/documents/{sha}", s.handleGetDocument)
	r.Get("/documents/{sha}/chunks", s.handleGetDocumentChunks)

	r.Route("/{ws}/{team}/{proj}", func(r chi.Router) { … })
})
```

프로젝트 스코프 경로 파라미터는 `projectKey(r)` 하나로 뽑는다.

```go
func projectKey(r *http.Request) hotstore.ProjectKey {
	return hotstore.ProjectKey{
		Workspace: chi.URLParam(r, "ws"),
		Team:      chi.URLParam(r, "team"),
		Project:   chi.URLParam(r, "proj"),
	}
}
```

`/v1/documents/...`(정적)와 `/v1/{ws}/...`(파라미터)가 같은 세그먼트를 두고 겹치지만, chi는 정적 노드를 먼저 시도하고 하위 매칭이 실패하면 파라미터 노드로 되돌아간다(`chi/v5 tree.go findRoute`). 즉 `documents`라는 이름의 워크스페이스도 정상 라우팅된다.

### 3.1 episodic

| 메서드 | 경로 | 주요 파라미터 | 성공 | 실패 |
|---|---|---|---|---|
| POST | `/v1/{ws}/{team}/{proj}/episodes` | body `CreateEpisodeRequest` | 201 `{record, degraded?}` | 400 검증, 503 hot store 미구성, hot 쓰기 실패는 Kind 그대로(409 중복 id / 500 I/O) |
| GET | `/v1/{ws}/{team}/{proj}/episodes/search` | `q`(필수) · `from` · `to`(RFC3339) · `kinds`(콤마) | 200 `[]search.Hit` | 400 검증, 503 인덱스 불가, 500 기타 |
| GET | `/v1/{ws}/{team}/{proj}/episodes/{id}` | `id`(ULID) | 200 `episodic.Record` | 400, 503 hot store 미구성, 404 hot·cold 모두 없음, 500 |

- **쓰기 계약**: 서버가 ULID를 발급하고 `consolidated:false`로 고정한다. 시각과 id는 둘 다 주입된 협력자에서 나온다 — `now := s.clock.Now().UTC()` → `s.ids.GenerateAt(now.UnixMilli())`. 핸들러는 `time.Now()`를 직접 부르지 않으므로 테스트에서 고정 시각/고정 id로 검증된다. `occurred_at`이 zero면 `now`로 채운다. hot append가 성공해야만 201이고, OpenSearch 업서트는 best-effort — 실패 시 `degraded:["search unavailable"]`을 실어 보내고 매니페스트에 dirty 마킹만 한다. 성공하면 `markIndexed`로 dirty 해제 + 평면 hydration sha 갱신.
- 검사 순서는 `validateProjectKey` → `s.store == nil`(503) → `decodeJSON` → `req.validate()`다. hot store가 없으면 body를 읽기도 전에 503이 나간다.
- **body 검증**(`CreateEpisodeRequest.validate`): `episodic.ValidKind` / `episodic.ValidActor`가 어휘를 소유한다 — `kind ∈ {event, conversation, decision, observation, document_chunk}`, `actor ∈ {agent, user, system}`, `text` 비어 있으면 안 됨. `kind=document_chunk`일 때만 `refs` 필수(`refs.doc_sha`는 `blob.ValidSHA`, `chunk_seq >= 0`), 그 외 kind에 `refs`가 있으면 400.
- **검색 계약**: 쿼리 파라미터 검증과 `s.index == nil` 확인을 통과한 뒤, 실제 `Search` 호출 직전에 `statGate`(재수화 디바운스, [07-rehydration](07-rehydration.md))를 돌린다 — 400/503으로 끝날 요청에 재수화 비용을 쓰지 않는다. 응답 `Hit`은 `record`(본문 `text`는 `query.go`가 `rec.Text = ""`로 비운다) + `excerpt` + `score`뿐이다 — 본문 전문 주입 없음. `Query.Size`를 핸들러가 설정하지 않으므로 상한은 `search.DefaultSearchSize = 20`건.
- 검색 성공 후 히트한 레코드의 `recall_count`/`last_recalled`를 best-effort로 올린다(`bumpRecall`). 실패해도 응답은 그대로 200. 성공하면 이어서 `convergeEpisodes`가 그 레코드들을 hot에서 다시 읽어 재색인한다 — `recall_count`·`last_recalled`·`consolidated`가 매핑된 색인 필드이고 hydration sha가 hot 파일 전체를 덮기 때문에, 이 되먹임이 없으면 첫 검색 이후 `CheckDrift`가 영구히 "episodic hot content changed"로 굳는다.
- **조회 계약**: hot 조회가 `errs.ErrNotFound`이면 `Archiver.FetchArchivedEpisode`로 S3 아카이브를 뒤진다. 에피소드가 cold로 가라앉아도 provenance 링크가 계속 해석되도록 하기 위함([03-lifecycle](03-lifecycle.md)). archiver 자체가 없으면 `404 "episode not in hot store; cold archive unavailable"`.

```bash
# 저장
curl -sS -X POST http://127.0.0.1:8420/v1/vms/ai/memory-mcp/episodes \
  -H 'Content-Type: application/json' \
  -d '{"kind":"decision","actor":"user",
       "occurred_at":"2026-08-25T10:00:00Z",
       "text":"블랙박스 검증은 실버킷 프리픽스를 쓰기로 결정했다",
       "entities":["blackbox","s3"]}'
# {"success":true,"data":{"record":{"id":"01K...","kind":"decision","consolidated":false,
#   "recall_count":0,"last_recalled":""}}}

# 검증 실패 — 에러 봉투
curl -sS -X POST http://127.0.0.1:8420/v1/vms/ai/memory-mcp/episodes \
  -H 'Content-Type: application/json' -d '{"kind":"nope","actor":"user","text":"x"}'
# {"success":false,"data":null,"error":{"code":"invalid_request",
#   "message":"kind must be one of event|conversation|decision|observation|document_chunk"}}

# 형태소 검색 (nori — "끄는"으로 저장된 "끄고"를 찾는다)
curl -sS -G http://127.0.0.1:8420/v1/vms/ai/memory-mcp/episodes/search \
  --data-urlencode 'q=보안을 끄는' \
  --data-urlencode 'kinds=decision,observation' \
  --data-urlencode 'from=2026-08-01T00:00:00Z'
```

### 3.2 knowledge

| 메서드 | 경로 | 주요 파라미터 | 성공 | 실패 |
|---|---|---|---|---|
| POST | `…/knowledge/nodes` | body `CreateNodeRequest` | 201 `{node, degraded?}` | 400, 503, 404 supersede 대상 없음, **409 id 중복**(`supersedes` 경로에서 `knowledge.Supersede`가 판정), 500 |
| PATCH | `…/knowledge/nodes/{id}` | body `PatchNodeRequest` | 200 `{node, degraded?}` | 400, 503, 404, **409 불법 전이**, 500 |
| DELETE | `…/knowledge/nodes/{id}` | `confirm=true`(필수) | 200 `{purged_id, removed_edges, degraded?}` | 400 confirm 누락, 503, 404, **409 active 노드**, 500 |
| POST | `…/knowledge/edges` | body `knowledge.Edge` | 201 `{edge, degraded?}` | 400, 503, 404 끝점 노드 없음, 500 |
| GET | `…/knowledge/search` | `q`(필수) · `include_archived` | 200 `[]knowledge.Node` | 400, 503, 500 |
| GET | `…/knowledge/graph` | `entity`(필수) · `depth`(1~10) | 200 `knowledge.Graph` | 400, 404 entity 없음, 503, 500 |

- **노드 생성**: `knowledge.ValidNodeKind`(`entity|fact|lesson|preference|document`), `name` 비어 있으면 안 되고, `knowledge.ValidTrust`(`user-stated|agent-inferred|imported`). `provenance`/`supersedes`의 각 원소는 ULID여야 한다. `review_after`는 RFC3339이거나 빈 문자열. 상태는 항상 `active`로 시작한다.
- **읽기-판정-쓰기가 한 클로저 안**이다. 핸들러는 `s.store.UpdateKnowledge(ctx, key, func(g) (g, error))` 안에서 조회·상태기계·삽입을 모두 끝낸다 — 문서가 통째로 교체되는 저장 방식이라, 읽고 나서 쓰는 두 단계로 나누면 동시 생성이 hot에서 노드를 지운 채 Neo4j에만 남기는 파생-전용 콘텐츠가 생긴다.
- `supersedes`가 비어 있지 않으면 그 클로저 안에서 `knowledge.Supersede(g, node, ids, now)`를 적용한다 — 판정은 호출자 몫이고 서버는 시키는 대로만 한다. 상태기계는 `knowledge`가 소유하므로 대상 노드 없음은 그쪽의 `errs.NotFound`(→404), 자기 자신 supersede는 `errs.Invalid`(→400), id 중복은 `errs.Conflict`(→409)로 나가고 `apierr.From`이 매핑한다.
- **쓰기 순서**는 전 엔드포인트 동일: hot JSON 원자 쓰기 성공 → Neo4j MERGE는 best-effort(`mirrorKnowledge`). Neo4j가 죽어 있으면 `degraded:["graph unavailable"]` + dirty 마킹, 그래도 201/200.
- **provenance 승격**: 노드 생성이 끝나면 `promoteProvenance`가 `provenance`에 이름 붙은 hot 에피소드들을 `consolidated:true`로 표시한다. 증류를 선언한 것은 에이전트이고 서버는 그 결과만 기록한다([03-lifecycle](03-lifecycle.md) §3.1의 cold 이동 전제). 이미 cold로 갔거나 다른 프로젝트인 id는 조용히 건너뛰고, 실패하면 `degraded:["provenance episodes not marked consolidated"]`를 실을 뿐 201을 취소하지 않는다. 표시에 성공하면 `convergeEpisodes`로 색인까지 맞춘다.
- **엣지**: `from`/`to`는 ULID, `knowledge.ValidRel`(`relates_to|derived_from|supersedes|about`), `confidence ∈ [0,1]`, 양끝 노드가 hot 그래프에 실제로 있어야 한다(없으면 404 — 이 판정도 삽입과 같은 클로저 안이다). `(from, to, rel)` 조합은 멱등 — 다시 POST하면 기존 엣지를 교체한다(`upsertEdge`).
- **PATCH**: `op`는 `set_state`(`knowledge.ValidState` — `active|archived|deprecated`) 또는 `deprecate`. 목표 상태가 `deprecated`면 `reason`이 필수다. 전이 합법성은 `knowledge.Transition`이 판정하고, 불법 전이는 `errs.Conflict`이므로 `apierr.From`이 **409**로 매핑한다(예: `deprecated → archived`는 거부, `deprecated → active` 부활은 허용).
- **DELETE(purge)**: `confirm=true` 검사가 **id 검증보다 먼저** 실행된다. 그리고 `confirm=true`만으로는 부족하다 — `knowledge.Purge`가 `active` 노드를 `errs.Conflict`로 거부하므로 **409**가 나간다. §3의 "삭제 대신 상태 전이" 버퍼(archived/deprecated)를 지나야 지울 수 있다. 삭제되면 그 노드에 걸린 모든 엣지를 hot에서 제거하고 제거된 엣지 수를 반환하며, 남은 노드들의 `supersedes`/`superseded_by` 참조도 함께 수선한다. Neo4j에서도 best-effort로 지운다. 복구는 S3 버저닝이 백스톱이다([02-storage-model](02-storage-model.md)).
- **검색/순회**: 둘 다 파라미터 검증과 `s.graph == nil` 확인을 지난 뒤, Neo4j 호출 직전에 `statGate`를 돈다. `include_archived`는 문자열 `"true"`일 때만 참으로 본다. Cypher 전문검색은 `searchLimit = 50`, 순회 깊이는 서버에서 1~10으로 강제되고 Neo4j 쪽 `clampDepth`가 한 번 더 조인다([05-knowledge-graph](05-knowledge-graph.md)).

```bash
# 사실 등록 + 기존 사실 supersede
curl -sS -X POST http://127.0.0.1:8420/v1/vms/ai/memory-mcp/knowledge/nodes \
  -H 'Content-Type: application/json' \
  -d '{"kind":"fact","name":"S3 리전","body":"버킷 리전은 ap-northeast-2 이다",
       "trust":"user-stated","provenance":["01K3ABC..."],"supersedes":["01K3OLD..."]}'

# 상태 전이 (reason 없으면 400)
curl -sS -X PATCH http://127.0.0.1:8420/v1/vms/ai/memory-mcp/knowledge/nodes/01K3OLD... \
  -H 'Content-Type: application/json' \
  -d '{"op":"deprecate","reason":"프로파일 기본 리전과 혼동한 오기록"}'

# 이웃 순회
curl -sS -G http://127.0.0.1:8420/v1/vms/ai/memory-mcp/knowledge/graph \
  --data-urlencode 'entity=OpenSearch' --data-urlencode 'depth=2'

# purge — confirm 없으면 400, active 노드면 409
curl -sS -X DELETE 'http://127.0.0.1:8420/v1/vms/ai/memory-mcp/knowledge/nodes/01K3OLD...?confirm=true'
```

### 3.3 documents

| 메서드 | 경로 | 주요 파라미터 | 성공 | 실패 |
|---|---|---|---|---|
| POST | `/v1/{ws}/{team}/{proj}/documents` | multipart `file` | 201 `document.IngestResult` | 400, 503 파이프라인/S3 없음, 500 |
| GET | `/v1/documents/{sha}` | `sha`(64 hex) | 200 원본 바이트 | 400, 503, 404, 500 |
| GET | `/v1/documents/{sha}/chunks` | `sha`(64 hex) | 200 `[]episodic.Record` | 400, 503, 404, 500 |

- 조회 두 개는 **프로젝트 스코프 밖**이다. blob은 content-addressed라 sha만으로 전역 유일하다.
- 인제스트는 `Documents`와 `Archiver`가 **둘 다** 있어야 한다. S3가 없으면 `503 "cold storage unavailable"` — §6의 blob 업로드가 cold-first라서 S3 없이는 계약이 성립하지 않기 때문이다. 파이프라인 자체가 없으면 `503 "document pipeline unavailable"`.
- 업로드 상한: `http.MaxBytesReader`로 128 MiB, `ParseMultipartForm` 메모리 임계 32 MiB. 초과하면 `400 "multipart form unreadable or too large"`.
- 폼 필드 이름은 `file` 고정이고 파일명이 비면 400.
- 응답 `IngestResult`는 `sha`·`blob_key`·`node_id`·`extractable`·`chunk_ids[]`·`truncated?`·`degraded?`. 텍스트 레이어가 없는 스캔 PDF는 `extractable:false`로 **정직하게** 보고하고 서버가 OCR로 추측하지 않는다. 청크 색인이나 document 노드 MERGE가 실패해도 hot 쓰기는 이미 끝났으므로 201을 유지하되 `degraded`에 그 사실을 적는다([06-documents](06-documents.md)).
- 원본 다운로드는 로컬 blob 캐시 우선, 미스 시 S3 재수화. 스트리밍 도중 끊기면 헤더가 이미 나갔으므로 로그만 남긴다.

```bash
# 인제스트
curl -sS -X POST http://127.0.0.1:8420/v1/vms/ai/memory-mcp/documents \
  -F 'file=@architecture-v2.pdf'
# {"success":true,"data":{"sha":"9f2b…","blob_key":"jin/blobs/9f2b…",
#   "node_id":"01K…","extractable":true,"chunk_ids":["01K…","01K…"]}}

# 원본 바이트 (봉투 아님)
curl -sS -o restored.pdf http://127.0.0.1:8420/v1/documents/9f2b…

# 이 문서에서 나온 청크 에피소드
curl -sS http://127.0.0.1:8420/v1/documents/9f2b…/chunks
```

### 3.4 ops

| 메서드 | 경로 | 주요 파라미터 | 성공 | 실패 |
|---|---|---|---|---|
| GET | `/healthz` | — | 200 `{"status":"ok"}` | — |
| GET | `/v1/status` | — | 200 `StatusReport` | 503 hot store 미구성, 500 매니페스트 불가 |
| POST | `/v1/consolidate` | body `ConsolidateRequest`(선택) | 200 `consolidate.Report` | 400 프로젝트 셀렉터 오류, 503, 500 |
| POST | `/v1/reindex` | `verify=true`(선택) | 200 `rehydrate.Report` | 503, 500 |

- `/healthz`는 의존성을 하나도 보지 않는다. 프로세스가 살아 있으면 200 — liveness 전용이고 readiness가 아니다. 실제 건강 상태는 `/v1/status`가 말한다.
- `StatusReport`는 정직성 계약 그 자체다: `drift`(episodic/knowledge 각각 `detected`·`reason`·`unavailable`), `unconsolidated`, `stale_unconsolidated`(TTL 초과했는데 미통합이라 가라앉지 못하는 건수), `manifest_updated_at`, `dirty_files[]`(정렬됨), `s3{reachable,bucket,last_archive_at,last_snapshot_at}`, `degraded[]`.
- 드리프트를 물어볼 수 없을 때도 "드리프트 없음"으로 답하지 않는다. rehydrator가 없으면 `unavailable:true, reason:"rehydrator not configured"`, 점검 자체가 실패하면 `reason:"drift check failed"`.
- S3 도달성 판정은 `FetchBlob(sha256(""))` 프로브 한 방이다 — 성공이거나 `errs.ErrNotFound`면 도달 가능. **HEAD가 아니라 GET인 것이 의도된 설계**다: S3는 존재하지 않는 버킷의 `HeadObject`에도 키 없음과 구별 불가능한 맨 404를 주기 때문에, HEAD 프로브는 없는 버킷을 "reachable"로 보고한다. `GetObject`는 `NoSuchBucket`을 준다.
- 미통합 카운트는 전 프로젝트의 hot 에피소드를 훑는다. 부분 실패는 `degraded`에 문자열(`"unconsolidated count unavailable"`, `"unconsolidated count incomplete: ws/team/proj"`)로 남기고 `/status` 자체는 계속 200을 답한다.
- `/v1/consolidate`의 body는 **비어 있어도 된다**(`decodeJSON(..., allowEmpty=true)`). `projects`는 `"ws/team/proj"` 문자열 배열이며 형식이 틀리면 400. `dry_run:true`면 무엇이 가라앉을지만 계산한다.
- 통합 실행 후 `ArchiveKeys`/`SnapshotKeys`가 비어 있지 않으면 `/status`가 보고하는 `last_archive_at`/`last_snapshot_at`을 갱신한다(`recordColdActivity` — 프로세스 메모리, `statusMu`로 보호되며 이 패키지의 유일한 락이다).
- `/v1/reindex`는 `verify` 쿼리가 정확히 `"true"`일 때만 전량 해시 감사를 켠다.

```bash
curl -sS http://127.0.0.1:8420/healthz
# {"success":true,"data":{"status":"ok"}}

curl -sS http://127.0.0.1:8420/v1/status
# {"success":true,"data":{"drift":{"episodic":{"detected":false,"unavailable":false},…},
#   "unconsolidated":12,"stale_unconsolidated":0,"dirty_files":[],
#   "s3":{"reachable":true,"bucket":"vms-memory-mcp",…},"degraded":[]}}

curl -sS -X POST http://127.0.0.1:8420/v1/consolidate \
  -H 'Content-Type: application/json' \
  -d '{"projects":["vms/ai/memory-mcp"],"dry_run":true}'

curl -sS -X POST 'http://127.0.0.1:8420/v1/reindex?verify=true'
```

---

## 4. 경계 검증과 상한값

`internal/server/validate.go`가 HTTP 경계 검증의 배선(파라미터 파싱·크기 상한·에러 변환)을 모아 두고, **어휘 자체는 도메인 패키지가 소유한다** — 서버는 그 판정 함수를 호출만 한다. 엣지 검증은 그래프 대수와 함께 `knowledge_graph.go`에 있다. 모든 검증 헬퍼는 `*apierr.Error`를 돌려주므로 실패 경로가 상태 코드를 다시 고를 일이 없다.

| 대상 | 규칙 | 소유자 | 위반 시 |
|---|---|---|---|
| `{ws}` `{team}` `{proj}` | `^[a-z0-9._-]+$`, 빈 문자열·`.`·`..` 금지 | `keySegmentPattern` (+ `hotstore.ProjectKey.Validate`가 자기 경계에서 재검사) | 400 |
| `{sha}` · `refs.doc_sha` | `^[0-9a-f]{64}$` | `blob.ValidSHA` | 400 |
| `{id}`(episode·node) | ULID | `ulid.Valid` | 400 |
| `provenance` / `supersedes` 원소 | 전부 ULID | `ulid.Valid` (`validateULIDs`) | 400, 어긋난 값을 문구에 포함 |
| `kind` / `actor` | 알려진 값 | `episodic.ValidKind` / `episodic.ValidActor` | 400 |
| 노드 `kind` / `state` / `trust`, 엣지 `rel` | 알려진 값 | `knowledge.ValidNodeKind` / `ValidState` / `ValidTrust` / `ValidRel` | 400 |
| JSON body | `maxJSONBodyBytes = 1 MiB`, 빈 body 금지(`/v1/consolidate` 제외) | `decodeJSON` | 400 |
| multipart | `maxUploadBytes = 128 MiB`, `multipartMemoryBytes = 32 MiB` | `handleIngestDocument` | 400 |
| `depth` | `defaultGraphDepth = 1`, `maxGraphDepth = 10` | `parseDepthParam` | 400 |
| `from` / `to` | RFC3339 | `parseTimeParam` | 400 |
| `kinds` | 콤마 분리, 각 값이 알려진 kind | `parseKindsParam` | 400 |
| `confidence` | `[0,1]` | `validateEdge` | 400 |

- `decodeJSON`은 `Content-Type`을 **검사하지 않는다**. 본문이 유효한 JSON이면 통과한다. JSON 파싱 에러 문구는 그대로 되돌려 준다 — 호출자가 보낸 바이트를 설명할 뿐 서버 내부를 흘리지 않기 때문이다.
- `normalizeStrings`는 문자열 배열의 공백을 다듬고 빈 값을 버리며 **항상 non-nil 슬라이스**를 돌려준다 — hot JSON에 `null` 배열이 남지 않도록.
- 불리언 쿼리(`confirm`, `verify`, `include_archived`)는 전부 `== "true"`(`valueTrue`) 정확 비교다. `1`·`TRUE`·`yes`는 false로 취급된다.

---

## 5. degraded 규칙 — 쓰기는 살고 파생 읽기는 죽는다

`server.Config`의 협력자는 두 부류다. `Store`·`Index`·`Graph`·`Documents`·`Consolidator`·`Rehydrator`·`Archiver`는 인터페이스이고 **nil일 수 있다** — 파생 저장소가 죽어 있어도 서버는 부팅한다([07-rehydration](07-rehydration.md), [10-operations](10-operations.md)). 반면 `Clock`·`IDs`·`Logger`는 **필수**이고 `New`가 없으면 거절한다(`errs.Invalid`): degrade될 수 있는 서비스가 아니라 첫 요청에서 패닉을 낼 원시 의존이기 때문이다.

| 상황 | 쓰기 엔드포인트 | 파생 읽기 엔드포인트 |
|---|---|---|
| OpenSearch 불가 | 201 + `degraded:["search unavailable"]` + dirty 마킹 | `GET …/episodes/search` → 503 |
| Neo4j 불가 | 201/200 + `degraded:["graph unavailable"]` + dirty 마킹 | `GET …/knowledge/search`·`graph` → 503 |
| Archiver 미구성(nil) | 문서 인제스트만 503 `cold storage unavailable` | `GET …/episodes/{id}`의 cold 폴백 불가 → 404 |
| provenance 승격 실패 | 201 + `degraded:["provenance episodes not marked consolidated"]` | — |
| hot store 불가 | 503 `hot store unavailable` | 503 |

위 표 셋째 줄은 **Archiver를 아예 만들지 못한** 경우다. Archiver는 있는데 **S3가 죽어 있는** 경우는 다르게 끝난다 — `cold`가 `KindUnavailable`을 올리고 `apierr.From`이 503으로 옮긴다(`GET /v1/documents/{sha}`, `GET …/episodes/{id}`의 아카이브 폴백 모두 404가 아니라 503). `/v1/status`는 이때 `s3.reachable:false` + `degraded:["cold storage unavailable"]`로 보고하고 자신은 200을 유지한다.

`degraded.go`의 문구 상수는 두 묶음이다.

- **degraded 노트 4종** — `"search unavailable"`, `"graph unavailable"`, `"cold storage unavailable"`, `"provenance episodes not marked consolidated"`.
- **협력자 자체가 없을 때의 503 문구 4종** — `"hot store unavailable"`, `"document pipeline unavailable"`, `"consolidation unavailable"`, `"rehydrator unavailable"`. 정직한 부분 응답이 성립하지 않는 경우라 degraded 노트가 아니라 503이다.

앞의 세 degraded 노트는 503 본문과 **같은 문자열**을 쓴다 — 쓰기가 실었을 노트와 읽기가 답하는 에러가 한 어휘다.

---

## 6. swaggo — 주석에서 스펙, 스펙에서 UI

**① 전역 API 정보**는 `cmd/memory-mcp/main.go`의 주석 블록에 있다.

```go
//	@title			memory-mcp v2 API
//	@version		2.0
//	@description	Local personal memory server: episodic (OpenSearch) + knowledge (Neo4j) …
//	@host			127.0.0.1:8420
//	@BasePath		/
```

**② 엔드포인트 주석**은 각 핸들러 위 `godoc` 블록에 붙는다. 응답은 제네릭 합성으로 쓴다.

```go
//	@Success	200	{object}	Envelope{data=[]search.Hit}
//	@Failure	400	{object}	Envelope
//	@Failure	503	{object}	Envelope	"index unavailable (§5)"
//	@Router		/v1/{ws}/{team}/{proj}/episodes/search [get]
```

이 표기는 `swagger.json`에서 `allOf: [ {$ref: server.Envelope}, {properties: {data: {$ref: …}}} ]`로 전개된다. 봉투 구조가 스펙에서도 그대로 보이고, `server.Envelope.error`는 `apierr.Error`(`code`·`message`·`details?`)를 참조한다.

**③ 생성**은 `make swagger`.

```make
swagger: ## Regenerate docs/ (swagger.json + docs.go) from swag annotations
	$(GO) run github.com/swaggo/swag/cmd/swag init -g cmd/memory-mcp/main.go -o docs --parseInternal
	$(GO) run github.com/swaggo/swag/cmd/swag fmt -d internal/server,cmd/memory-mcp
```

- `--parseInternal`이 필수다. DTO(`server.*`)와 도메인 타입(`episodic.Record`, `knowledge.Node`, `search.Hit`, `document.IngestResult`, `consolidate.Report`, `rehydrate.Report`, `apierr.Error` …)이 전부 `internal/` 아래에 있어서, 이 플래그가 없으면 정의가 비어 버린다.
- swag CLI 버전은 `tools.go`(`//go:build tools`)가 `go.mod`에 못 박는다 — 현재 `github.com/swaggo/swag v1.16.6`.
- 산출물은 `docs/docs.go`, `docs/swagger.json`, `docs/swagger.yaml`. 현재 스펙에는 **16개 오퍼레이션과 32개 definition**이 들어 있고, `/swagger/*` 자신은 문서화 대상이 아니다.

**④ 등록**은 blank import 하나로 끝난다.

```go
_ "github.com/drakejin/memory-mcp/docs" // swag-generated OpenAPI spec
```

`docs.init()`이 `swag.Register(SwaggerInfo.InstanceName(), SwaggerInfo)`를 호출하고(인스턴스 이름 `"swagger"`), 핸들러는 그 인스턴스를 읽는다.

**⑤ 서빙**은 `r.Get("/swagger/*", httpSwagger.WrapHandler)` 한 줄이다. `swaggo/http-swagger/v2 v2.0.2`의 기본 설정(`URL: "doc.json"`)으로 동작하며 경로 꼬리표에 따라 갈린다.

| 요청 | 응답 |
|---|---|
| `/swagger/index.html` | Swagger UI HTML(내장 템플릿) |
| `/swagger/doc.json` | `swag.ReadDoc("swagger")` 결과 = 생성된 OpenAPI 2.0 문서 |
| `/swagger/swagger-ui-bundle.js` 등 | `swaggo/files/v2`에 임베드된 정적 자산 |
| `/swagger/` | `index.html`로 301 리다이렉트 |
| GET 이외 메서드 | chi가 먼저 `405`(빈 본문 + `Allow: GET`)로 거절한다 — 라우터에 `r.Get`만 등록돼 있어 `WrapHandler` 자신의 `Method not allowed` 응답까지 가지 않는다 |

블랙박스 시나리오 1이 부팅 직후 `GET /swagger/doc.json`이 200이고 OpenAPI 문서로 파싱되는지 검사한다([11-testing](11-testing.md)).

---

## 7. ⚠️ 설계 문서와 차이

설계 문서 §7과 실제 코드가 어긋나거나, 문서가 말하지 않은 실제 동작들.

1. **swaggo 주석의 응답 코드는 실제 반환 코드의 부분집합이다.** 거의 모든 핸들러가 의존성 nil일 때 503을, 내부 실패에 500을 반환하지만 주석에는 빠져 있다. 예: `POST …/episodes`는 `400/500`만 선언하는데 `Store == nil`이면 503, `GET …/episodes/{id}`는 `400/404`만 선언하는데 503·500도 낼 수 있고, `GET /v1/status`는 200만 선언하지만 503/500을 낸다. `docs/swagger.json`을 클라이언트 생성기에 그대로 먹이면 이 경로들이 누락된다.
2. **설계 문서의 "`include_archived` 옵트인"은 knowledge 검색에만 있다.** episodic 검색에는 해당 파라미터가 없다 — episodic record에는 state 개념 자체가 없기 때문이다. 문서의 recall 문단이 두 평면을 뭉뚱그린 표현이다.
3. **"모든 응답 `{success, data, error}` 봉투"에는 예외가 둘 있다.** `GET /v1/documents/{sha}`의 성공 응답(원본 바이트)과, 라우터 레벨 404/405 — 404는 chi 기본 `text/plain`(`404 page not found`)이고 405는 본문이 아예 없다(§2).
4. **에러 봉투의 `error`는 문자열이 아니라 객체이고, `"data": null`이 항상 들어간다.** 설계 문서 §7은 봉투의 세 필드만 규정할 뿐 `error`의 타입도 `data` 필드의 존재 여부도 정하지 않는다. 코드는 `{"code","message","details?"}`(code-standards §2.2의 `apierr.Error` 공개 투영)로 고정했다.
5. 설계 문서 §7 표는 `DELETE …/knowledge/nodes/{id}?confirm=true`만 적지만, 코드는 **confirm 검사를 id 검증보다 먼저** 수행하고(잘못된 id + confirm 누락이면 에러 문구가 confirm 쪽으로 나온다), 그 위에 **`active` 노드는 409로 거부**한다. 후자는 §3의 "삭제 대신 상태 전이" 원칙을 §7 표가 옮겨 적지 않은 것으로, 설계 문서 두 절 사이의 간극이다.
6. **`apierr.Error.Details`는 스키마에 노출되지만 프로덕션 핸들러 중 채우는 곳이 없다.** `swagger.json`의 `apierr.Error`에 `details`가 있고 `WithDetail`도 구현돼 있지만 호출자는 테스트뿐이라, 현재 클라이언트가 실제로 받는 것은 언제나 `{code, message}`다.

---

## 8. 코드 위치

| 개념 | 파일 | 심볼 |
|---|---|---|
| 라우트 트리 | `internal/server/server.go` | `(*Server).Router` |
| 서버 수명주기·설정 | `internal/server/server.go` | `New`, `Config`, `(Config).validate`, `(*Server).ListenAndServe` |
| 협력자 인터페이스(소비자 측 narrow) | `internal/server/deps.go` | `HotStore`, `EpisodeIndex`, `KnowledgeGraph`, `DocumentIngestor`, `Consolidator`, `Rehydrator`, `ColdArchive`, `Clock`, `IDGenerator` |
| 부팅 드리프트 점검 | `internal/server/startup.go` | `(*Server).Startup` |
| 응답 봉투·전송 에러 생성 | `internal/server/respond.go` | `Envelope`, `writeJSON`, `writeAPIError`, `badRequest`, `notFound`, `unavailable` |
| Kind→HTTP 단일 변환점 | `internal/server/apierr/apierr.go` | `Error`, `From`, `New`, `WithCause`, `WithDetail`, `publicMessage`, `CodeInvalidRequest` 외 4종 |
| 도메인 에러 어휘 | `internal/errs/errs.go` | `Kind`, `Error`, `ErrNotFound` 외 4종, `Invalid`/`NotFound`/`Conflict`/`Unavailable`/`Internal`/`Wrap`/`IO`/`FromContext`, `WithField`, `LogValue` |
| 경계 검증·상한 | `internal/server/validate.go` | `validateProjectKey`, `validateSHA`, `validateULID`, `validateULIDs`, `decodeJSON`, `parseTimeParam`, `parseKindsParam`, `parseDepthParam`, `parseProjectString`, `normalizeStrings`, `maxUploadBytes` |
| degraded 문구·매니페스트 | `internal/server/degraded.go` | `degradedSearch`, `degradedGraph`, `degradedCold`, `degradedPromotion`, `msgHotStoreUnavailable` 외 3종, `markDirty`, `markIndexed`, `statGate` |
| episodic 핸들러 | `internal/server/handlers_episodic.go` | `handleCreateEpisode`, `handleSearchEpisodes`, `handleGetEpisode`, `bumpRecall`, `convergeEpisodes` |
| knowledge 핸들러 | `internal/server/handlers_knowledge.go` | `handleCreateNode`, `handleCreateEdge`, `handlePatchNode`, `handlePurgeNode`, `handleSearchKnowledge`, `handleKnowledgeGraph`, `mirrorKnowledge`, `promoteProvenance` |
| 그래프 대수·엣지 검증 | `internal/server/knowledge_graph.go` | `findGraphNode`, `affectedNodes`, `diffEdges`, `validateEdge`, `upsertEdge`, `nodeExists` |
| documents 핸들러 | `internal/server/handlers_documents.go` | `handleIngestDocument`, `handleGetDocument`, `handleGetDocumentChunks` |
| ops 핸들러 | `internal/server/handlers_ops.go` | `handleStatus`, `handleConsolidate`, `handleReindex`, `StatusReport`, `S3SyncStatus`, `coldReachable`, `recordColdActivity` |
| 전역 swagger 주석·조립 | `cmd/memory-mcp/main.go` | `@title`/`@host`/`@BasePath`, `build`, `_ "…/docs"` |
| 바인딩 강제 | `internal/config/config.go` | `DefaultListenAddr`, `(Config).Validate`, `isLoopback` |
| 생성된 스펙 | `docs/docs.go`, `docs/swagger.json`, `docs/swagger.yaml` | `SwaggerInfo`, `docTemplate` |
| swag CLI 핀 | `tools.go`, `Makefile` | `//go:build tools`, `make swagger` |
| 봉투·에러 형태 테스트 | `internal/server/validate_test.go` | `TestEnvelopeShape`, `TestNewRejectsUnservableConfig`, `TestEveryRouteIsAnnotated`(16 오퍼레이션 · 15 path 고정), `TestUnknownRouteIs404` |
| 핸들러 테스트 하네스·페이크 | `internal/server/fakes_test.go` | `newTestServer`, `do`, `fakeClock`/`fakeIDs`/`fakeStore` 외 |
| API 블랙박스 | `test/blackbox/blackbox_test.go`, `harness_test.go` | `TestScenario01_Startup` 외 7종, `postJSON`, `postMultipart`, `projPath` |
