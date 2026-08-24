# memory-mcp

에이전트의 장기 기억을 관리하는 로컬 메모리 서버. 기억을 "저장했다가 꺼내는 것"이 아니라 **태어나고, 회상되고, 증류되고, 가라앉는 라이프사이클**로 다룬다.

![memory-mcp 시스템 개요](docs/spec/assets/00-hero.svg)

## 핵심 아이디어

에이전트의 기억은 성격이 다른 두 종류가 섞여 있다. 하나는 **사건**이다 — "그때 무슨 일이 있었나". 다른 하나는 **지식**이다 — "지금 무엇이 참인가". 사건은 계속 쌓이고 낡으며 언젠가 증류되어야 하고, 지식은 영구히 남되 개정되고 뒤집힌다. 이 둘을 한 저장소에 밀어 넣으면 둘 다 어중간해진다.

memory-mcp는 이 둘을 **분리된 평면**으로 다루고, 저장 위치를 **정본과 파생물**로 다시 나눈다.

| | episodic (사건) | knowledge (지식) |
|---|---|---|
| 담는 것 | 대화·작업 로그·관찰·문서 청크 | 개체·사실·교훈·관계 |
| 수명 | 유한 — 증류 후 cold로 가라앉음 | 영구 — 삭제 대신 상태 전이 |
| 변경 | append-only, 정정은 새 record | 비파괴 개정, supersede 체인 |
| 파생 인덱스 | OpenSearch (nori 한국어 형태소) | Neo4j (그래프 순회) |

그리고 **정본은 언제나 로컬 JSON 파일**이다. OpenSearch와 Neo4j는 볼륨 없는 컨테이너 위에서 돌아가는 파생물이라 언제 죽어도 되고, 죽으면 hot에서 다시 만들어진다. 파생물에만 존재하는 콘텐츠는 없다.

세 가지 원칙이 전부를 관통한다:

1. **원본이 콘텐츠를 소유한다** — 인덱스는 언제든 폐기하고 재구성할 수 있는 f(원본)이다.
2. **판정 없는 자동 덮어쓰기는 없다** — 충돌은 감지·표면화까지가 서버의 일이고, 판정은 호출 에이전트의 몫이다. 서버 안에 LLM은 없다.
3. **자기 한계를 1급으로 보고한다** — 인덱스 신선도, 미통합 건수, 잘린 청크, 드리프트를 응답이 항상 말한다. 보고 없음 ≠ 완전함.

## 빠르게 실행하기

Go 1.25+, Docker, 그리고 S3에 접근할 AWS 프로파일이 필요하다.

```bash
make start
```

이 한 줄이 세 가지를 순서대로 한다: 파생 저장소 컨테이너 기동 → 둘 다 healthy가 될 때까지 대기 → 서버 실행. 뜨고 나면:

- API — <http://127.0.0.1:8420>
- Swagger UI — <http://127.0.0.1:8420/swagger/index.html>
- 상태 확인 — `make status`

```bash
# 사건 하나 기록
curl -sX POST http://127.0.0.1:8420/v1/vms/platform/memory-mcp/episodes \
  -H 'content-type: application/json' \
  -d '{"kind":"decision","actor":"agent","text":"파생 저장소는 볼륨 없이 띄우고 재수화로 복구하기로 했다","entities":["memory-mcp"]}'

# 조사 변형을 넘어서 검색된다 (nori 형태소)
curl -s 'http://127.0.0.1:8420/v1/vms/platform/memory-mcp/episodes/search?q=재수화하는'

# 시스템이 자기 상태를 정직하게 보고한다
curl -s http://127.0.0.1:8420/v1/status
```

## 스펙 문서

설계의 정본은 `docs/`에 있다. 주제별로 나뉘어 있고, 각 문서는 실제 코드를 기준으로 쓰였다 — 코드와 설계가 어긋나면 코드가 이기고, 그 격차는 각 문서의 "설계 문서와 차이" 절에 기록된다. 읽는 순서와 문서 규약은 [스펙 인덱스](docs/spec/README.md)에 있다.

### 시작하기

| 문서 | 내용 |
|---|---|
| [01 · 시스템 개요](docs/spec/01-overview.md) | 이중 평면과 3계층 저장, 요청이 흐르는 경로, 비목표 |
| [10 · 운영](docs/spec/10-operations.md) | `make` 타깃, 컨테이너 구성, 환경변수, 트러블슈팅 |

### 저장과 생명주기

| 문서 | 내용 |
|---|---|
| [02 · 저장 모델](docs/spec/02-storage-model.md) | hot 디렉토리 레이아웃, 레코드 스키마, manifest, S3 키 구조 |
| [03 · 메모리 생명주기](docs/spec/03-lifecycle.md) | 상태기계, supersede, hot→cold 에이징 규칙, consolidation |
| [06 · 문서](docs/spec/06-documents.md) | blob·추출·청킹, PDF 처리, 잘림의 정직한 보고 |

### 검색과 그래프

| 문서 | 내용 |
|---|---|
| [04 · Episodic 검색](docs/spec/04-episodic-search.md) | OpenSearch 매핑, nori 형태소, 질의 구성, 발췌 응답 계약 |
| [05 · Knowledge 그래프](docs/spec/05-knowledge-graph.md) | 노드·엣지 모델, Cypher 패턴, supersede 체인, provenance |

### 시스템 성질

| 문서 | 내용 |
|---|---|
| [07 · 재수화와 degraded](docs/spec/07-rehydration.md) | 드리프트 감지, 부분 재수화, 파생물이 죽었을 때의 동작 |
| [08 · HTTP API](docs/spec/08-http-api.md) | 전체 엔드포인트, 응답 봉투, Swagger 생성 |
| [09 · 코드 구조](docs/spec/09-code-structure.md) | 패키지 경계, Client 인터페이스 관용구, 2계층 에러 설계 |
| [11 · 검증](docs/spec/11-testing.md) | 단위 테스트 전략, 블랙박스 수용 기준 8단계 |

### 설계 배경

| 문서 | 내용 |
|---|---|
| [architecture-v2.md](docs/design/architecture-v2.md) | v2 설계 정본 — 무엇을 만드는가 |
| [code-standards.md](docs/design/code-standards.md) | 코드 규약 정본 — 어떻게 쓰는가 |
| [architecture.md](docs/design/architecture.md) | v1 설계 (마크다운 + SQLite). 원칙의 출처 |
| [analyze/](docs/analyze/) | 세 소스 분석 — codebase-memory-mcp 실측, Codebase-Memory 논문, MemOS 논문 |

## API 표면

모든 응답은 `{success, data, error}` 봉투를 쓴다. 검색은 **경로·발췌·점수 구성요소만** 돌려주고 본문은 주입하지 않는다 — 질의당 토큰이 1급 성능 지표다.

| 그룹 | 엔드포인트 |
|---|---|
| episodic | `POST /v1/{ws}/{team}/{proj}/episodes`<br>`GET /v1/{ws}/{team}/{proj}/episodes/search`<br>`GET /v1/{ws}/{team}/{proj}/episodes/{id}` |
| knowledge | `POST .../knowledge/nodes` · `POST .../knowledge/edges`<br>`GET .../knowledge/search` · `GET .../knowledge/graph`<br>`PATCH .../knowledge/nodes/{id}` · `DELETE .../knowledge/nodes/{id}` |
| documents | `POST /v1/{ws}/{team}/{proj}/documents`<br>`GET /v1/documents/{sha}` · `GET /v1/documents/{sha}/chunks` |
| 운영 | `POST /v1/consolidate` · `POST /v1/reindex` · `GET /v1/status` · `GET /healthz` |

상세는 [08 · HTTP API](docs/spec/08-http-api.md) 또는 실행 후 Swagger UI에서 볼 수 있다.

## 설정

| 환경변수 | 기본값 | 역할 |
|---|---|---|
| `DJ_MEMORY_HOME` | `~/.local/dj-memory` | hot 정본 저장소 루트 |
| `DJ_MEMORY_USERNAME` | OS 사용자 | S3 키 프리픽스 (`s3://버킷/{username}/`) |
| `DJ_MEMORY_S3_BUCKET` | `vms-memory-mcp` | cold 저장소 버킷 |
| `DJ_MEMORY_S3_REGION` | `ap-northeast-2` | 버킷 리전 — **프로파일 기본 리전을 상속하지 않는다** |
| `AWS_PROFILE` | `vms-holdings` | S3 자격증명 프로파일 |
| `DJ_MEMORY_OPENSEARCH_URL` | `http://127.0.0.1:9200` | episodic 인덱스 |
| `DJ_MEMORY_NEO4J_URL` | `bolt://127.0.0.1:7687` | knowledge 그래프 |
| `DJ_MEMORY_EPISODIC_TTL_DAYS` | `30` | 통합된 episode가 cold로 내려가는 기한 |
| `DJ_MEMORY_LISTEN_ADDR` | `127.0.0.1:8420` | 바인드 주소 (루프백 고정) |

## 개발

```bash
make help       # 타깃 목록
make test       # 단위 테스트 — 컨테이너도 AWS도 필요 없음 (fake 주입)
make blackbox   # 수용 기준 8단계 — 실제 컨테이너 + 실제 S3
make swagger    # swag 주석으로부터 docs/swagger.json 재생성
make logs       # 파생 저장소 로그
make stop       # 컨테이너 정리
```

### 저장소 구조

```
cmd/memory-mcp/        서버 진입점
internal/
  config/              환경변수 로딩·검증
  hotstore/  blob/     정본 JSON 저장소, 원자 쓰기, manifest / 컨텐츠 주소 캐시
  episodic/  search/   사건 도메인 / OpenSearch 클라이언트
  knowledge/ graph/    지식 도메인·상태기계 / Neo4j 클라이언트
  document/            추출·청킹
  cold/                S3 아카이브
  consolidate/         증류 제안·에이징 파이프라인
  rehydrate/           드리프트 감지·재수화
  server/              go-chi 핸들러 + swagger 주석
deploy/                docker-compose (OpenSearch+nori, Neo4j — 볼륨 없음)
test/blackbox/         수용 기준 시나리오
docs/spec/             주제별 기술 스펙
docs/design/           설계 정본
```

`make test`가 컨테이너 없이 도는 이유는 외부 의존이 전부 인터페이스 뒤에 있고 테스트가 같은 인터페이스를 구현한 fake를 주입하기 때문이다. 실제 저장소에 대고 확인해야 하는 것은 [블랙박스 수용 기준](docs/spec/11-testing.md)이 담당한다.

## 이 프로젝트의 이력

v1은 TypeScript로 마크다운 원본 + SQLite 파생 인덱스 구조였고, 한국어 검색을 prefix·trigram 조합으로 **근사**했다. v2는 Go로 다시 쓰면서 그 근사를 nori 형태소 분석으로 정면 해결하고, 지식의 관계를 그래프로 옮기고, 저장을 hot/cold 계층으로 나눴다. 바뀐 것은 구현이고, 바뀌지 않은 것은 원칙이다 — 원본이 콘텐츠를 소유하고, 판정은 위임되며, 시스템은 자기 한계를 보고한다.

v1 코드는 `src/`에 역사적 참조로 남아 있다.

## License

MIT
