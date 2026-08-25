# 10 — 운영: 기동·설정·인프라

`make start` 한 줄로 파생 컨테이너 2개를 띄우고 헬스체크를 기다린 뒤 호스트에서 서버를 실행하기까지, 실제 코드가 읽는 설정·포트·자격증명과 실패했을 때 보이는 문자열을 정리한다.

| 항목 | 내용 |
|---|---|
| 관련 코드 | [`Makefile`](../../Makefile) · [`deploy/docker-compose.yml`](../../deploy/docker-compose.yml) · [`deploy/opensearch/Dockerfile`](../../deploy/opensearch/Dockerfile) · [`internal/config/config.go`](../../internal/config/config.go) · [`cmd/memory-mcp/main.go`](../../cmd/memory-mcp/main.go) · [`internal/errs/errs.go`](../../internal/errs/errs.go) · [`internal/server/apierr/apierr.go`](../../internal/server/apierr/apierr.go) · [`internal/cold/cold.go`](../../internal/cold/cold.go) · [`internal/server/startup.go`](../../internal/server/startup.go) |
| 관련 스펙 | [01-overview](01-overview.md) · [02-storage-model](02-storage-model.md) · [03-lifecycle](03-lifecycle.md) · [07-rehydration](07-rehydration.md) · [08-http-api](08-http-api.md) · [09-code-structure](09-code-structure.md) · [11-testing](11-testing.md) · 인덱스: [README](README.md) |
| 상태 | 구현 완료 — Makefile 17개 타깃, compose 서비스 2개, env 9개 모두 코드에 존재. 설계 근거는 [architecture-v2.md](../design/architecture-v2.md) §8(인프라)·§5(재수화)·§10.1(기동 수용 기준), 에러 표현은 [code-standards.md](../design/code-standards.md) §2 |

![로컬 운영 구성: 호스트에서 실행되는 memory-mcp 서버(127.0.0.1:8420), docker compose가 띄운 볼륨 없는 OpenSearch(9200)와 Neo4j(7687/7474) 컨테이너, 그리고 머신 밖의 S3 버킷 vms-memory-mcp(ap-northeast-2)](assets/10-operations.svg)

---

## 1. make 타깃

### 1.1 변수

`Makefile` 상단의 3개가 전부다. 전부 `?=`이므로 환경변수나 `make VAR=...`로 덮어쓸 수 있다.

| 변수 | 기본값 | 쓰이는 곳 |
|---|---|---|
| `GO` | `go` | `start` / `run` / `build` / `vet` / `vet-blackbox` / `test` / `live` / `swagger` / `blackbox` |
| `COMPOSE` | `docker compose -f deploy/docker-compose.yml` | `status` / `logs` / `infra-up` / `infra-down` |
| `LISTEN` | `127.0.0.1:8420` | **`start`의 안내 문구 2줄과 `make status`의 curl 대상뿐** |

> **함정**: `LISTEN`은 서버에 전달되지 않는다. 서버가 실제로 바인딩하는 주소는 `DJ_MEMORY_LISTEN_ADDR`(§4)이다. `make LISTEN=127.0.0.1:9000 start`는 안내 문구만 9000으로 바뀌고 서버는 여전히 8420에 뜬다. 포트를 바꾸려면 **둘 다** 설정해야 한다.

### 1.2 타깃

| 타깃 | 선행 조건 | 실제 실행 | 비고 |
|---|---|---|---|
| `help` | — | `grep -hE '^[a-zA-Z_-]+:.*?## '` + awk `printf` | `## ` 주석이 달린 타깃만, **Makefile 등장 순서 그대로**(정렬 없음) |
| `start` | `infra-up` `infra-wait` | 안내 2줄 echo → `go run ./cmd/memory-mcp` | **포그라운드**. 아래 §2 참조 |
| `stop` | `infra-down` | 컨테이너만 내린다 | 서버는 포그라운드이므로 Ctrl-C |
| `restart` | `stop` `start` | 그대로 이어붙임 | `start`가 블로킹이라 `restart`도 블로킹 |
| `status` | — | `docker compose ps` + `curl -fsS http://$(LISTEN)/healthz` | 서버가 없으면 `not running` 출력 |
| `logs` | — | `docker compose logs -f` | 컨테이너 로그만. 서버 로그는 stderr |
| `infra-up` | — | `docker compose up -d --build` | `--build`이므로 nori 플러그인 이미지가 없으면 여기서 빌드 |
| `infra-down` | — | `docker compose down -v --remove-orphans` | `-v`는 정리 취향이 아니라 필수다 — 아래 §3.1 |
| `infra-wait` | — | `docker inspect -f '{{.State.Health.Status}}'` 폴링 | 3초 × 60회 = 최대 180초 |
| `run` | — | `go run ./cmd/memory-mcp` | 인프라가 이미 떠 있다고 가정. 안 떠 있어도 서버는 뜬다(저하 모드) |
| `build` | — | `go build ./...` | |
| `vet` | — | `go vet ./...` | |
| `vet-blackbox` | — | `go vet -tags blackbox ./test/blackbox/...` | 빌드 태그가 붙은 §10 스위트는 `go test ./...`에 **보이지 않으므로** 타입 체크를 따로 건다 |
| `test` | `vet-blackbox` | `go test -race ./...` | fake만 사용. 컨테이너·AWS 불필요. **레이스 디텍터가 기본** |
| `live` | `infra-up` `infra-wait` | `DJ_MEMORY_LIVE_TEST=1 go test -count=1 -v ./internal/search/... ./internal/graph/... ./internal/cold/...` | 실 컨테이너 + **실 S3 버킷** 사용. §4.4 |
| `swagger` | — | `go run .../swag init -g cmd/memory-mcp/main.go -o docs --parseInternal` + `swag fmt -d internal/server,cmd/memory-mcp` | §6.7 |
| `blackbox` | `infra-up` `infra-wait` | `go test -tags blackbox -count=1 -v ./test/blackbox/...` | 실 컨테이너 + **실 S3 버킷** 사용 → [11-testing.md](11-testing.md) |

`.PHONY`에 위 17개 타깃이 모두 등록되어 있다.

외부 의존성 기준으로 타깃을 셋으로 나누면:

- **아무것도 필요 없음** — `help`, `run`, `build`, `vet`, `vet-blackbox`, `test`, `swagger`. `run`은 컨테이너가 없어도 저하 모드로 뜬다(§2.2).
- **도커만** — `start`, `stop`, `restart`, `status`, `logs`, `infra-up`, `infra-down`, `infra-wait`.
- **도커 + 실 S3 자격증명** — `live`, `blackbox` 둘뿐이다.

즉 **`make test`는 컨테이너도 AWS도 요구하지 않는다** — 이것이 §4.4의 live 게이트가 지키는 성질이다.

---

## 2. `make start`가 실제로 하는 일

### 2.1 Make 단계

```
make start
 ├─ (1) infra-up    : docker compose -f deploy/docker-compose.yml up -d --build
 ├─ (2) infra-wait  : docker inspect 폴링 (3s × 60)
 ├─ (3) echo        : "==> derived stores healthy; starting memory-mcp on http://127.0.0.1:8420"
 │                    "==> swagger UI: http://127.0.0.1:8420/swagger/index.html"
 └─ (4) go run ./cmd/memory-mcp   ← 포그라운드, 여기서 블로킹
```

**(1) `infra-up`** — `--build`이므로 `deploy/opensearch/Dockerfile`을 먼저 빌드한다. 이 Dockerfile은 `opensearchproject/opensearch:2.19.1` 위에서 `opensearch-plugin install --batch analysis-nori`를 실행하므로 **첫 실행은 이미지 pull + 플러그인 다운로드로 수 분** 걸릴 수 있다. 이 시간은 `up`이 반환하기 전에 소모되므로 (2)의 180초 예산에는 포함되지 않는다.

**(2) `infra-wait`** — 컨테이너 이름을 하드코딩해서 헬스 상태를 폴링한다.

```make
@echo "==> waiting for opensearch + neo4j to become healthy"
@for i in $$(seq 1 60); do \
	os=$$(docker inspect -f '{{.State.Health.Status}}' dj-memory-opensearch 2>/dev/null || echo missing); \
	nj=$$(docker inspect -f '{{.State.Health.Status}}' dj-memory-neo4j 2>/dev/null || echo missing); \
	if [ "$$os" = healthy ] && [ "$$nj" = healthy ]; then echo "==> opensearch: healthy, neo4j: healthy"; exit 0; fi; \
	printf '\r    opensearch: %-9s neo4j: %-9s (%ss)' "$$os" "$$nj" "$$((i*3))"; \
	sleep 3; \
done; \
echo "\n!! containers did not become healthy — check 'make logs'"; exit 1
```

(`$$`는 make가 셸에 `$` 하나를 넘기기 위한 이스케이프다.)

- 컨테이너가 없으면 `docker inspect`가 실패하고 상태는 `missing`으로 표시된다.
- 180초 안에 둘 다 `healthy`가 되지 않으면 **exit 1**이므로 `make`가 여기서 멈추고 (4)는 실행되지 않는다.
- 컨테이너 쪽 헬스체크 예산은 `start_period 20s + interval 5s × retries 30 ≈ 170초`로, make의 180초와 대략 맞춰져 있다.

**(4) `go run ./cmd/memory-mcp`** — 컴파일 후 바이너리를 포그라운드로 실행한다. 서버가 종료될 때까지 make가 반환하지 않으므로, 컨테이너를 내리려면 **다른 셸에서** `make stop`을 실행한다.

### 2.2 서버 부팅 단계 (`cmd/memory-mcp/main.go`)

`main`은 로거를 만들고 `run(logger)`을 부른 뒤, 에러가 있으면 **한 줄만 찍고 종료**한다. 부팅 로직 전체가 `run` 안에 있는 이유는 주석에 명시돼 있다 — "so every deferred close still runs before main calls os.Exit".

```go
func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("memory-mcp exited", "error", err)
		os.Exit(exitFailure)   // exitFailure = 1
	}
}
```

| 순서 | 코드 | 실패하면 |
|---|---|---|
| 1 | `slog.New(slog.NewTextHandler(os.Stderr, nil))` → `slog.SetDefault` | — (모든 로그는 **stderr** 텍스트 포맷) |
| 2 | `config.Load()` | 에러를 `run`이 반환 → **`memory-mcp exited` + exit 1** |
| 3 | `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)` + `defer stop()` | — |
| 4 | `build(cfg, logger)` → `(srv, release, err)` + `defer release()` | 아래 표. 실패는 전부 exit 1 |
| 5 | `srv.Startup(ctx)` — `CheckDrift` → 드리프트 감지 시 `RehydrateAll(ctx, false)` | 로그만 남기고 계속. **절대 치명이 아니다** |
| 6 | `srv.ListenAndServe(ctx)` | 에러 반환 → **`memory-mcp exited` + exit 1** |

`build`는 **I/O를 전혀 하지 않는** 순수 조립 함수다(코드 규약 §1 rule 4). 순서와 실패 처리:

| 순서 | 코드 | 실패하면 |
|---|---|---|
| 4-1 | `hotstore.NewSystemClock()` | — (프로세스에서 `time.Now()`를 부르는 유일한 곳) |
| 4-2 | `ulid.New(ulid.Config{Clock: clock})` | 치명 — 프로세스당 하나만 두어 id가 전역 단조 증가한다 |
| 4-3 | `hotstore.New(hotstore.Config{Home: cfg.Home, Clock: clock})` | 치명. 디스크를 건드리지 않음 — 디렉터리는 첫 쓰기 때 생성 |
| 4-4 | `blob.New(blob.Config{Dir: filepath.Join(cfg.Home, "blobs")})` | 치명. 역시 디스크 미접근 |
| 4-5 | `search.New(search.Config{URL: cfg.OpenSearchURL, Logger: logger})` | `opensearch client init failed; episodic search degraded` **Warn**, `index`는 nil |
| 4-6 | `graph.New(graph.Config{URL, User, Password, Logger})` | `neo4j client init failed; knowledge graph degraded` **Warn**, `graphClient`는 nil |
| 4-7 | `cold.New(cold.Config{Bucket, Region, Profile, Username})` | `s3 client init failed; cold archive degraded` **Warn**, `archiver`는 nil |
| 4-8 | `document.New` / `consolidate.New` / `rehydrate.New` / `server.New` | 치명 — 각각 `release()` 후 에러 반환 |

**핵심 성질 4가지**

1. **4-5·4-6·4-7의 생성자는 네트워크를 건드리지 않는다.** `search.New`는 URL 공백 검사 + opensearch-go 클라이언트 생성만, `graph.New`는 bolt 드라이버 객체만("It does not dial; use Ping to verify"), `cold.New`는 `awsconfig.LoadDefaultConfig`(로컬 `~/.aws/*` 읽기)만 한다. 따라서 **컨테이너가 전부 죽어 있어도 `make run`은 성공**하고, 연결 실패는 첫 사용 시점 또는 `/v1/status`에서 드러난다.
2. **파생 저장소 부재는 저하(degraded)이지 실패가 아니다.** 셋 다 인터페이스 타입 변수에 담기고 실패 시 nil로 남으므로, 하류의 "설정돼 있나?" 검사가 진실을 읽는다 ([03-lifecycle.md](03-lifecycle.md), [07-rehydration.md](07-rehydration.md)).
3. **부팅 중 exit는 세 종류다** — config 검증 실패, `build` 조립 실패(4-2·4-3·4-4·4-8), 리스닝 실패. 셋 다 같은 로그 한 줄(`memory-mcp exited`)로 나온다. 앞의 둘은 `*errs.Error`이므로 원인이 `error.kind`/`error.op`/`error.cause.*` 그룹으로 펼쳐지고, 리스닝 실패는 net 패키지의 평범한 에러라 `error="listen tcp ..."` 한 문자열로만 나온다(§6.2·§6.3). `build` 조립 실패를 부팅 실패로 삼는 것은 의도다: Config 리터럴에서 필수 의존성이 빠진 것은 패키지 단위 유닛 테스트가 볼 수 없는 결함이라, 이 이음매가 최초 실행이 아니라 CI에서 터지게 만든다.
4. **Ctrl-C** → SIGINT → ctx 취소 → `httpServer.Shutdown`(10초 예산) → `run` 반환 → `defer release()`로 Neo4j 드라이버 풀 정리(`release`는 graph 클라이언트가 만들어졌을 때만 실 작업을 한다). `os.Exit`이 `main`에만 있으므로 **에러로 끝나는 경로에서도 defer는 실행된다** — 포트 충돌로 죽어도 드라이버는 정식으로 닫힌다. 닫기가 실패하면 `neo4j driver close failed` Warn.

`ListenAndServe`의 고정값: `readHeaderTimeout 5s`, 셧다운 유예 `shutdownGrace 10s`. 요청별 타임아웃은 **의도적으로 없다**(대용량 문서 인제스트는 수 초가 정상, §6). 기동 로그는 `memory-mcp listening addr=127.0.0.1:8420`.

---

## 3. docker-compose — 파생 저장소 2개

`deploy/docker-compose.yml`에는 서비스 2개뿐이고, **서버는 여기 없다**(호스트 실행).

| | `opensearch` | `neo4j` |
|---|---|---|
| container_name | `dj-memory-opensearch` | `dj-memory-neo4j` |
| 이미지 | `build: ./opensearch` → `opensearchproject/opensearch:2.19.1` + `analysis-nori` | `neo4j:5-community` |
| 포트 | `127.0.0.1:9200:9200` | `127.0.0.1:7474:7474`(HTTP 브라우저), `127.0.0.1:7687:7687`(bolt) |
| 환경 | `discovery.type=single-node`<br>`DISABLE_SECURITY_PLUGIN=true`<br>`DISABLE_INSTALL_DEMO_CONFIG=true`<br>`OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m` | `NEO4J_AUTH=neo4j/djmemory-local` |
| healthcheck | `curl -fsS http://localhost:9200/_cluster/health \| grep -qE '"status":"(green\|yellow)"'` | `cypher-shell -u neo4j -p djmemory-local 'RETURN 1' \|\| exit 1` |
| interval / timeout / retries / start_period | 5s / 5s / 30 / 20s | 5s / 10s / 30 / 20s |
| volumes | **선언 없음** | **선언 없음** |

**포트 바인딩이 전부 `127.0.0.1:` 접두사**라는 점이 중요하다. compose가 `0.0.0.0`에 노출하지 않으므로 같은 네트워크의 다른 기기가 인증 없는 OpenSearch/Neo4j에 접근할 수 없다. 서버의 loopback 강제(§4.2)와 같은 원칙이다.

### 3.1 볼륨이 없다는 것의 의미 — 그리고 `down -v`

의도적 비영속이다(설계 §11 비목표: "OpenSearch/Neo4j 데이터 볼륨 영속화"). 결과:

- `make stop && make start` 또는 `docker compose down && up` 이후 **인덱스 0건, 그래프 노드 0개**로 시작한다.
- 이때 콘텐츠가 사라지는 것이 아니다. 정본은 `~/.local/dj-memory/`의 JSON이고([02-storage-model.md](02-storage-model.md)), 부팅 시 `Startup`이 manifest와 대조해 드리프트를 감지하면 전체 재수화한다([07-rehydration.md](07-rehydration.md)).
- 그래서 "컨테이너를 지웠는데 검색이 안 된다"는 정상 상태가 아니라 **재수화가 아직 안 돌았거나 실패한 것**이다. `/v1/status`의 `drift.episodic.reason`이 이유를 말해준다.
- 반대로 **파생 저장소에만 존재하는 콘텐츠는 버그다.** 컨테이너를 마음대로 날려도 되는 것이 이 구조의 안전장치다.

**`infra-down`이 `-v`를 붙이는 이유**는 Makefile 주석이 직접 설명한다. compose 파일은 볼륨을 선언하지 않지만 **`neo4j` 이미지 자체가 `VOLUME /data /logs`를 선언**하므로, `up` 할 때마다 익명 볼륨 2개가 새로 생긴다. 평범한 `down`은 이것들을 남기고, 블랙박스를 반복 실행하면 도커 디스크가 차서 컨테이너 생성이 ENOSPC로 실패한다. 블랙박스 하니스의 `composeFresh`도 같은 이유로 `down -v --remove-orphans`를 쓴다(그쪽 주석은 실패가 다음 실행에서 가짜 "container name already in use"로 보인다는 것까지 적어 두었다).

OpenSearch 인덱스는 프로젝트별로 나뉘지 않고 `dj-memory-episodic` 하나를 `workspace`/`team`/`project` keyword 필드로 스코핑한다(`internal/search/mapping.go`). 한 번의 bulk 재수화로 전부 재구성하기 위한 설계다.

---

## 4. 환경변수 (`internal/config`)

### 4.1 표

`config.Load()`가 읽는 전부다. `envOr` 헬퍼는 **빈 문자열을 미설정으로 취급**하므로 `DJ_MEMORY_S3_BUCKET=`은 기본값으로 폴백한다(`DJ_MEMORY_HOME`·`DJ_MEMORY_USERNAME`·`DJ_MEMORY_EPISODIC_TTL_DAYS`도 전용 resolver가 같은 규칙을 쓴다).

| env | 기본값 (상수) | Config 필드 | 쓰이는 곳 |
|---|---|---|---|
| `DJ_MEMORY_HOME` | `~/.local/dj-memory` (`homeParentDir`/`homeDirName`) | `Home` | hot 정본 루트, `{Home}/blobs` 캐시 |
| `DJ_MEMORY_USERNAME` | `user.Current().Username` | `Username` | **모든 S3 키의 첫 세그먼트** — `cold.Config.Username`으로 주입 |
| `DJ_MEMORY_S3_BUCKET` | `vms-memory-mcp` (`DefaultS3Bucket`) | `S3Bucket` | `cold.New`, `/v1/status`의 `s3.bucket` |
| `DJ_MEMORY_S3_REGION` | `ap-northeast-2` (`DefaultS3Region`) | `S3Region` | `awsconfig.WithRegion` — §5.2 |
| `AWS_PROFILE` | `vms-holdings` (`DefaultAWSProfile`) | `AWSProfile` | `awsconfig.WithSharedConfigProfile` |
| `DJ_MEMORY_OPENSEARCH_URL` | `http://127.0.0.1:9200` (`DefaultOpenSearchURL`) | `OpenSearchURL` | `search.New` |
| `DJ_MEMORY_NEO4J_URL` | `bolt://127.0.0.1:7687` (`DefaultNeo4jURL`) | `Neo4jURL` | `graph.New` |
| `DJ_MEMORY_EPISODIC_TTL_DAYS` | `30` (`DefaultEpisodicTTLDays`) | `EpisodicTTLDays` | 에이징 판정(`consolidate`), `/v1/status`의 `stale_unconsolidated` |
| `DJ_MEMORY_LISTEN_ADDR` | `127.0.0.1:8420` (`DefaultListenAddr`) | `ListenAddr` | `http.Server.Addr` |

`AWS_PROFILE`은 이 프로젝트 전용 접두사를 쓰지 않는 유일한 변수다(AWS SDK 표준 이름을 그대로 재사용).

`Config` 구조체에는 **json 태그가 하나도 없다.** Neo4j 비밀번호를 들고 있어서 응답 본문이나 로그 라인으로 직렬화되면 안 되기 때문이며, 주석이 그 이유를 명시한다.

### 4.2 검증 규칙 (`Config.Validate`)

`Load()`는 마지막에 `Validate()`를 호출하고, 실패하면 `Config{}`와 `errs.Wrap("config.Load", err)`를 반환한다 → `run`이 그대로 올려보내고 main이 exit 1.

**`Load` 경로의 모든 실패는 `*errs.Error`다.** 규칙 위반은 `errs.Invalid`(KindInvalid), OS 신원을 해석하지 못한 것은 `errs.Internal`(KindInternal)이다. 어느 쪽이든 위반한 값은 메시지가 아니라 **구조화 필드**로 실린다(코드 규약 §2.1) — 사람이 읽는 문장에는 값이 섞이지 않는다.

필드가 로그의 어느 깊이에 붙는지는 감싸졌는지로 갈린다. `Validate()` 실패는 `Load`가 `errs.Wrap`으로 한 겹 감싸므로 `error.cause.<field>`, `Load` 자신의 단계(TTL 파싱·home/username 해석) 실패는 감싸지 않으므로 `error.<field>`에 그대로 붙는다(§6.2의 두 예시가 각각 그 형태다).

| 규칙 | Kind | 공개 메시지 | 붙는 필드 |
|---|---|---|---|
| `Home` non-empty | invalid | `home must be non-empty` | — |
| `Username` non-empty | invalid | `username must be non-empty` | — |
| `S3Bucket` non-empty | invalid | `s3 bucket must be non-empty` | — |
| `ListenAddr`가 `127.0.0.1:` 또는 `localhost:` 로 **시작** | invalid | `listen addr must bind loopback only` | `listen_addr` |
| `EpisodicTTLDays > 0` | invalid | `episodic ttl days must be positive` | `episodic_ttl_days` |
| TTL 파싱: `strconv.Atoi` 성공 **그리고** `> 0` (Validate 이전, `Load` 단계) | invalid | `DJ_MEMORY_EPISODIC_TTL_DAYS must be a positive integer` | `value` |
| home 해석 실패 (`os.UserHomeDir`) | internal | `internal error` | `env=DJ_MEMORY_HOME`, 원인은 `error.cause` |
| OS 사용자 해석 실패 (`user.Current`) | internal | `internal error` | `env=DJ_MEMORY_USERNAME`, 원인은 `error.cause` |

**loopback 검사는 접두사 문자열 비교**다. 따라서 `[::1]:8420`, `::1:8420`, `0.0.0.0:8420`, `192.168.x.x:8420`은 전부 거부된다. IPv6 loopback으로 띄울 방법은 없다 — 인증이 없는 로컬 도구이므로 의도적이다([08-http-api.md](08-http-api.md)). 같은 규칙이 `server.Config.validate()`에도 한 번 더 있어서, config를 우회해 `server.New`를 직접 부르는 경로도 라우팅 가능한 주소로는 만들어지지 않는다.

**검증하지 않는 값**: `S3Region`, `AWSProfile`, `OpenSearchURL`, `Neo4jURL`. 오타는 부팅 시 조용히 통과하고, 저하 모드 경고나 첫 요청 실패로만 드러난다. 단 `cold.New`는 자기 쪽 `Config.validate()`에서 `Region`이 비어 있으면 `s3 region must be set explicitly`로 거부한다 — §5.2의 규칙을 생성 시점에 구조적으로 못 박은 것이다.

### 4.3 env로 바꿀 수 없는 값

| 값 | 위치 | 비고 |
|---|---|---|
| Neo4j 계정 `neo4j` / `djmemory-local` | `config.DefaultNeo4jUser` / `DefaultNeo4jPassword` | compose의 `NEO4J_AUTH`와 짝. `Config.Neo4jUser`/`Neo4jPassword`에 항상 이 상수가 들어가며 env 훅이 없다. 로컬 고정이며 비밀이 아니다(주석: "the graph is a disposable derivative bound to loopback") |
| `MaxProjectFileBytes` = `5 << 20`(5 MiB) | `config.go` const | episodic 압박 임계 |
| `MaxProjectRecords` = 5000 | `config.go` const | episodic 압박 임계 |
| `DocumentChunkBytes` = 2048 | `config.go` const | 문서 청킹 목표 크기 ([06-documents.md](06-documents.md)) |
| `MaxDocumentChunks` = 500 | `config.go` const | 청크 상한, 초과분은 `truncated`로 정직 보고 |
| 인덱스 이름 `dj-memory-episodic` | `search.IndexName` (`search/mapping.go`) | 전역 단일 인덱스 |
| 검색 기본 size 20 | `search.DefaultSearchSize` (`search/search.go`) | |
| `readHeaderTimeout` 5s / `shutdownGrace` 10s | `server/server.go` const | |

즉 **튜닝 가능한 임계값은 TTL 하나뿐**이다.

### 4.4 테스트 전용 env

| env | 읽는 곳 | 조건 | 효과 |
|---|---|---|---|
| `DJ_MEMORY_LIVE_TEST` | `internal/search/live_test.go` · `internal/graph/live_test.go` · `internal/cold/live_test.go` | `== "1"` (정확히) | 실 OpenSearch / 실 Neo4j / 실 S3 통합 테스트 실행. `make live`가 설정한다 |
| `BLACKBOX_KEEP` | `test/blackbox/harness_test.go` | `!= ""` | 성공해도 임시 `DJ_MEMORY_HOME`과 `server.log`를 남긴다 |

**live 게이트는 레포 전체에서 하나로 통일돼 있다** — 이름도 `DJ_MEMORY_LIVE_TEST`, 판정도 정확히 `"1"`, 세 패키지가 각자 같은 `liveEnvVar`/`liveEnvValue` 상수 쌍을 두고 그 사실을 주석으로 서로 참조한다(코드 규약 §3: 매직 문자열 금지, 균일한 패턴). 게이트가 꺼져 있으면 전부 `t.Skip`이므로 `make test`는 컨테이너·자격증명 없이 통과한다.

live 테스트가 실 자원을 건드릴 때 쓰는 격리 규칙:

| 패키지 | 격리 방식 | 추가 override env |
|---|---|---|
| `search` | 스크래치 인덱스 `dj-memory-episodic-livetest`를 쓰고 끝나면 drop — 운영 인덱스는 건드리지 않는다 | `DJ_MEMORY_OPENSEARCH_URL` |
| `graph` | 실 컨테이너에 붙고 `t.Cleanup`으로 드라이버를 닫는다 | `DJ_MEMORY_NEO4J_URL` |
| `cold` | username을 `jin/livetest/{테스트명}-{나노초 정밀도 UTC 타임스탬프}`(`livePrefix` + `t.Name()`의 `/`→`-`)로 만들어 키 네임스페이스를 분리하고, `t.Cleanup`이 `aws s3 rm --recursive`로 지운다 | — |

`internal/cold/live_test.go`의 `TestLiveWrongRegionFailsLoudly`는 §5.2의 리전 함정을 회귀 테스트로 못 박아 둔 것이다 — `awsconfig.WithRegion`을 지우는 리팩터는 유닛 커버리지를 초록으로 유지한 채 모든 아카이브를 깨뜨리기 때문이다.

---

## 5. AWS — cold store

### 5.1 준비물

| 항목 | 값 |
|---|---|
| 프로파일 | `vms-holdings` (`~/.aws/config`의 `[profile vms-holdings]`) |
| 버킷 | `vms-memory-mcp` |
| 리전 | `ap-northeast-2` |
| 버저닝 | **on** — purge의 백스톱 ([03-lifecycle.md](03-lifecycle.md)) |
| 퍼블릭 액세스 | 차단 |

버킷이 없을 때 블랙박스 하니스가 로그의 `fix` 필드에 담아 주는 복구 명령(실제로는 ` && `로 이어붙인 한 줄이다):

```bash
aws --profile vms-holdings s3api create-bucket \
  --bucket vms-memory-mcp --region ap-northeast-2 \
  --create-bucket-configuration LocationConstraint=ap-northeast-2 \
&& aws --profile vms-holdings s3api put-bucket-versioning \
  --bucket vms-memory-mcp --versioning-configuration Status=Enabled
```

서버는 버킷을 만들지 않는다. `cold.New`는 자격증명 로딩만 하고, 실제 키는 `internal/cold/keys.go`가 `{username}/episodic/{ws}/{team}/{proj}/{yyyy-mm}.json`, `{username}/knowledge/{ws}/{team}/{proj}/latest.json`(+ `snapshots/{ts}.json`), `{username}/blobs/{sha[:2]}/{sha}` 형태로 조립한다. 이 헬퍼들이 cold 경로의 유일한 출처이며, 패키지 안팎 어디서도 키를 손으로 붙이지 않는다.

### 5.2 리전 명시 함정 (중요)

```go
options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.Region)}
if cfg.Profile != "" {
    options = append(options, awsconfig.WithSharedConfigProfile(cfg.Profile))
}
// LoadDefaultConfig only parses ~/.aws/{config,credentials}; the context is
// demanded by the SDK signature and carries no network deadline here.
awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), options...)
```

`WithRegion`이 프로파일의 리전보다 우선한다. `config.go`의 상수 주석이 이유를 명시한다:

> `DefaultS3Region is the region of bucket vms-memory-mcp (§8). It is set explicitly rather than inherited from the AWS profile, whose default region differs — inheriting it yields PermanentRedirect on every call.`

설계 문서 §8은 프로파일 기본 리전을 `ap-southeast-1`로 특정한다. **프로파일 리전을 상속하면 버킷이 있는 리전과 달라 모든 S3 호출이 `PermanentRedirect`로 실패한다.** 그래서 `DJ_MEMORY_S3_REGION`은 "비워두면 알아서 되는" 값이 아니라 코드에 하드 기본값을 둔 값이고, `cold.Config.validate()`가 빈 리전을 아예 거부한다. 다른 리전의 버킷을 쓰려면 `DJ_MEMORY_S3_BUCKET`과 `DJ_MEMORY_S3_REGION`을 **반드시 함께** 바꿔야 한다.

`awsconfig.LoadDefaultConfig`가 `context.Background()`를 쓰는 것도 의도다 — 여기엔 네트워크 데드라인이 없고, 하는 일은 `~/.aws/{config,credentials}` 파싱뿐이다.

### 5.3 블랙박스가 쓰는 실 버킷 프리픽스

블랙박스 하니스는 서버 프로세스에 env 7개(`DJ_MEMORY_HOME`, `DJ_MEMORY_USERNAME`, `DJ_MEMORY_S3_BUCKET`, `DJ_MEMORY_S3_REGION`, `AWS_PROFILE`, `DJ_MEMORY_OPENSEARCH_URL`, `DJ_MEMORY_NEO4J_URL`)를 주입하고, `DJ_MEMORY_LISTEN_ADDR`은 주지 않아 기본 `127.0.0.1:8420`을 쓴다. `DJ_MEMORY_USERNAME=jin/blackbox-test`이고 `cold/keys.go`가 username을 키 첫 세그먼트로 쓰기 때문에 모든 객체가 `s3://vms-memory-mcp/jin/blackbox-test/` 아래로 떨어지며, `TestMain`이 실행 전후로 `aws s3 rm --recursive`로 청소한다. 하니스는 시작 시 `aws s3api head-bucket`으로 프리플라이트를 돌려 자격증명·버킷 문제를 **시나리오 중간이 아니라 시작 지점에서** 실패시킨다.

---

## 6. 트러블슈팅

### 6.1 `!! containers did not become healthy — check 'make logs'`

`infra-wait`가 180초를 넘겼다. 진행 표시줄에 찍힌 마지막 상태로 원인을 나눈다.

| 마지막 상태 | 원인 | 확인 |
|---|---|---|
| `missing` | 컨테이너가 아예 안 떴다 (compose 실패, 포트 충돌) | `docker compose -f deploy/docker-compose.yml ps -a`, `make logs` |
| `starting` 고정 | 부팅 중 OOM 또는 느린 초기화 | `docker inspect -f '{{json .State.Health}}' dj-memory-opensearch` |
| `unhealthy` | 헬스체크 명령 자체가 실패 | 위 명령의 `Log[].Output` |

자주 나오는 것들:

- **OpenSearch 메모리** — `-Xms512m -Xmx512m`이지만 컨테이너 총 사용량은 그보다 크다. Docker Desktop 메모리 할당이 부족하면 부팅 중 죽는다.
- **Linux 호스트의 `vm.max_map_count`** — OpenSearch는 최소 262144를 요구한다. `sudo sysctl -w vm.max_map_count=262144`.
- **포트 선점** — 다른 OpenSearch/Elasticsearch가 이미 9200을, 다른 Neo4j가 7687을 쓰고 있으면 compose가 바인딩에 실패한다.
- **Neo4j 헬스체크 타임아웃** — `cypher-shell`은 첫 부팅 시 느리다. `timeout: 10s`로 이미 완화되어 있으나 느린 디스크에서는 재시도 30회를 다 쓸 수 있다.
- **디스크 부족(ENOSPC)** — `down` 을 `-v` 없이 반복해 익명 볼륨이 쌓인 경우(§3.1). 증상이 "container name already in use"로 위장돼 나타나기도 한다. `docker volume prune`으로 정리한다.

### 6.2 서버가 부팅 즉시 exit 1

부팅 실패는 종류를 가리지 않고 **로그 한 줄**(`memory-mcp exited`)로 나온다. 원인이 `*errs.Error`이면 `error.*` 구조화 필드로 펼쳐진다 — 이 형태는 `internal/errs`의 `(*Error).LogValue()`가 만든다(코드 규약 §2.1). 실제 출력:

```
level=ERROR msg="memory-mcp exited" error.kind=invalid error.op=config.Load \
  error.cause.kind=invalid error.cause.op=config.Config.Validate error.cause.entity=config \
  error.cause.msg="listen addr must bind loopback only" error.cause.listen_addr=0.0.0.0:8420
```

```
level=ERROR msg="memory-mcp exited" error.kind=invalid error.op=config.Load error.entity=config \
  error.msg="DJ_MEMORY_EPISODIC_TTL_DAYS must be a positive integer" error.value=abc
```

읽는 법 (코드 규약 §2.1 — `*errs.Error`의 `LogValue`):

- `error.kind` — `invalid` / `not_found` / `conflict` / `unavailable` / `internal` 중 하나(`errs.Kind` 상수 5개). 부팅 실패의 대부분은 `invalid`(설정)이다.
- `error.op` — `config.Load`, `config.Config.Validate`, `server.New` 처럼 **스택이 아니라 경로**. 어디서 인식된 실패인지 바로 보인다.
- `error.cause.*` — 중첩된 도메인 원인. 원인이 또 `*errs.Error`이면 중첩 그룹으로, 외부 패키지 에러면 `error.cause="..."` 문자열 하나로 붙는다. `errs.Wrap`은 자기 메시지를 만들지 않으므로, 사람이 읽을 문장은 항상 가장 안쪽 것이다.
- `error.<field>` — 위반한 실제 값(`listen_addr`, `value`, `episodic_ttl_days`, `env`). **메시지가 아니라 필드에 있다.** 필드는 키 사전순으로 정렬돼 나온다.

`error.op=config.Load`가 보이면 env부터, `error.op=server.New`나 `rehydrate.New` 같은 것이 보이면 `cmd/memory-mcp/main.go`의 `build()` 조립이 깨진 것이다(§2.2 4-8).

**`error.*` 그룹이 아예 없고 `error="..."` 한 줄만 보이는 경우**는 도메인 에러가 아닌 것이 그대로 올라온 것이다. 부팅 경로에서 이런 형태가 나오는 곳은 `ListenAndServe`가 반환하는 net 에러 하나뿐이다(§6.3).

### 6.3 `memory-mcp exited` + `address already in use`

이전 실행이 살아 있다. 블랙박스 하니스가 쓰는 방식이 그대로 유효하다.

```bash
lsof -ti :8420 | xargs kill -9
```

이 실패는 **§6.2의 `error.*` 그룹으로 나오지 않는다.** `ListenAndServe`가 반환하는 것은 `*errs.Error`가 아니라 net 패키지의 에러라, `main`의 `logger.Error("memory-mcp exited", "error", err)`가 그것을 문자열 하나로 찍는다. 실제 출력:

```
level=INFO  msg="memory-mcp listening" addr=127.0.0.1:8420
level=ERROR msg="memory-mcp exited" error="listen tcp 127.0.0.1:8420: bind: address already in use"
```

바인딩은 부팅 마지막 단계이므로 `memory-mcp listening` 줄이 **먼저 찍히고 나서** 죽는다 — 이 두 줄이 붙어 있으면 설정이 아니라 포트 문제다. `make status`의 `server:` 줄로 먼저 확인한다. 단 §1.1의 `LISTEN` 함정에 주의 — 서버를 다른 포트로 띄웠다면 `make status`는 `not running`이라고 거짓말한다.

### 6.4 `opensearch client init failed` / `neo4j client init failed` / `s3 client init failed`

전부 **Warn이고 서버는 계속 뜬다.** 해당 의존성이 nil로 남아 저하 모드가 된다.

| 죽은 것 | 쓰기 | 읽기 | `/v1/status` |
|---|---|---|---|
| OpenSearch | 201 + `degraded: ["search unavailable"]` + 해당 파일 dirty 표시 | `GET .../episodes/search` → **503** `search unavailable` | `degraded`에 `search unavailable`, `drift.episodic.unavailable=true` |
| Neo4j | 200/201 + `degraded: ["graph unavailable"]` | `GET .../knowledge/search`, `.../knowledge/graph` → **503** `graph unavailable` | `degraded`에 `graph unavailable`, `drift.knowledge.unavailable=true` |
| S3 | 문서 업로드는 **503** `cold storage unavailable`(§6 step 2가 cold-first라 우회 불가) | — | `s3.reachable=false`, `degraded`에 `cold storage unavailable` |

저하 문구는 `internal/server/degraded.go`의 const 블록에 한 번만 선언돼 있고(파생 저장소 3종 `degradedSearch` / `degradedGraph` / `degradedCold`, 그리고 파생 저장소와 무관한 네 번째 `degradedPromotion`), 쓰기의 `degraded` 배열과 읽기의 503 메시지가 **같은 문자열**을 쓴다. 클라이언트가 어휘 하나만 알면 되도록 한 설계다.

이 어휘가 상태 코드로 바뀌는 지점은 두 계층으로 갈라져 있다(코드 규약 §2):

- 파생 저장소가 **아예 없을 때**(`s.index == nil` 등)와 도메인이 `errs.KindUnavailable`을 돌려줬을 때는, 핸들러가 `respond.go`의 `unavailable(degradedSearch)`로 503을 직접 만든다 — 저하 노트와 토씨 하나까지 같은 문장을 쓰기 위해서다.
- 그 밖의 도메인 실패는 `apierr.From(err)` 한 곳에서만 `errs.Kind` → HTTP 상태로 매핑된다(`invalid`→400, `not_found`→404, `conflict`→409, `unavailable`→503, 나머지→500). 응답 본문에는 `*errs.Error`의 `Msg`만 실리고 `Fields`와 원인은 로그로만 간다.

`drift.*.reason`이 원인을 구분해준다 (`internal/rehydrate/rehydrate.go`의 상수, 그리고 `handlers_ops.go`의 `driftReport`). 아래는 **생성자 실패·미도달과 직결된 것들만** 추린 것이고, 내용 변경으로 인한 드리프트 사유까지 포함한 전체 목록은 [07-rehydration.md](07-rehydration.md)에 있다:

| reason | 의미 |
|---|---|
| `opensearch not configured` / `neo4j not configured` | 생성자가 실패해 의존성이 nil — **부팅 로그에 Warn이 있다** |
| `opensearch unreachable` / `neo4j unreachable` | 객체는 있는데 `Ping` 실패 — URL 형식은 맞고 서비스가 죽었거나 주소가 틀렸다 |
| `episodic index absent or uncountable` | 붙긴 했는데 인덱스가 없다(새 컨테이너) → 재수화 대상 |
| `rehydrator not configured` / `drift check failed` | `/v1/status`가 질문 자체를 못 던진 경우. 두 평면 모두 `unavailable=true`로 보고 |

생성자가 다이얼하지 않으므로 **형식이 멀쩡한 URL 오타는 부팅 로그에 안 나온다.** `http://127.0.0.1:9999`는 조용히 통과하고 `unreachable`로만 드러난다. S3도 프로파일이 존재하면 부팅은 통과하고, 만료된 자격증명은 첫 `Put`/`Get`에서 터진다.

`/v1/status`의 S3 도달성 확인은 `sha256("")` = `e3b0c442...b855`를 키로 **`FetchBlob`(GET)** 을 던진다. HEAD가 아닌 이유가 코드 주석에 있다 — S3는 **존재하지 않는 버킷**에 대한 `HeadObject`에도 키 부재와 구분 불가능한 맨 404를 주므로, HEAD 기반 프로브는 없는 버킷을 "도달 가능"으로 보고해 버린다. `GetObject`는 `NoSuchBucket`을 주므로, 히트이거나 명시적 not-found일 때만 버킷이 실제로 있다고 판정한다. 실패 시 `cold store unreachable` Warn을 남긴다.

### 6.5 `PermanentRedirect` / `301` on S3

리전이 버킷과 다르다. §5.2. `DJ_MEMORY_S3_REGION`을 비우면 코드 기본값 `ap-northeast-2`로 돌아간다 — 프로파일 리전을 상속시키지 말 것. 회귀 방지 테스트는 `make live`의 `TestLiveWrongRegionFailsLoudly`.

### 6.6 한국어 검색이 안 잡힌다

로그에 이 줄이 있는지 본다.

```
level=WARN msg="episodic index lacks the nori mapping; dropping and recreating" index=dj-memory-episodic analyzer=korean
```

원인은 **`_bulk`가 인덱스를 자동 생성한 경우**다. 매핑 없는 동적 인덱스에는 `korean` 분석기가 없어 형태소 매칭("보안을 끄고" ↔ "보안을 끄는")이 조용히 깨지고, `id`가 text가 되어 정렬도 망가진다.

방어책은 `internal/search/index.go`의 `ensureSchema`이고, `_bulk`를 던지는 두 경로(`IndexRecords` / `DeleteRecords`)가 배치 전에 이것을 먼저 부른다(`internal/search/bulk.go`). 동작은 2단계다:

1. **아직 래치 전이면** — `HEAD /{index}`로 존재를 확인하고, 있으면 `GET /{index}/_settings`를 읽어 `korean` 분석기 유무를 본다. 없으면 위 Warn을 찍고 **인덱스를 drop한 뒤 재생성**한다(파생물이므로 허용되는 복구, §5). 성공하면 `schemaReady`를 래치한다.
2. **래치된 뒤에도** — 배치마다 `HEAD` 한 번은 계속 던진다. 래치는 *이 프로세스가 마지막에 본 것*만 기록하는데, 파생 저장소는 서버가 도는 중에도 교체될 수 있는 일회용 컨테이너다. 래치만 믿으면 컨테이너 교체 후 래치는 "수렴됨"이라 말하고 다음 `_bulk`가 nori 없는 인덱스를 자동 생성해, **에러 하나 없이** 한국어 재현율이 죽는다. 그래서 비싼 `_settings` 읽기만 래치하고 존재 확인은 남겨 두었다. `HEAD`가 부재를 보고하면 래치를 풀고 nori 매핑으로 다시 만든다.

같은 패키지의 `DeleteProject`(부분 재수화의 삭제 경로)는 `_bulk`가 아니라 `_delete_by_query`를 쓰고 인덱스를 자동 생성하지 않으므로 `ensureSchema`를 부르지 않는다 — 인덱스가 없으면 지울 것도 없다는 뜻이라 그대로 성공 처리한다.

그래도 안 되면:

1. `curl 127.0.0.1:9200/dj-memory-episodic/_settings | grep korean` 으로 분석기 존재 확인
2. 없으면 `POST /v1/reindex`로 강제 전체 재수화
3. nori 플러그인 자체가 빠진 경우 — `make infra-down && make infra-up`으로 이미지를 다시 빌드 (`docker exec dj-memory-opensearch bin/opensearch-plugin list`로 확인)

자세한 매핑은 [04-episodic-search.md](04-episodic-search.md).

### 6.7 swagger 관련

- `make swagger`는 `docs/docs.go`, `docs/swagger.json`, `docs/swagger.yaml`을 재생성한다.
- `cmd/memory-mcp/main.go`가 `_ "github.com/drakejin/memory-mcp/docs"`로 blank import하므로 **`docs/`를 지우면 빌드가 깨진다.** 지웠다면 `make swagger`로 되살린다.
- swag CLI 버전은 `tools.go`(build tag `tools`)가 go.mod에 고정한다(`swaggo/swag v1.16.6`). 전역 설치본이 아니라 레포 고정 버전이 `go run`으로 쓰인다.
- `--parseInternal`이 필요하다. 응답 타입 대부분이 `internal/` 아래에 있어 이 플래그 없이는 스펙이 비어 나온다.
- 확인: `curl -s 127.0.0.1:8420/swagger/doc.json | head -c 100` — 블랙박스 `TestScenario01_Startup`이 이걸 검증한다(200 + `"swagger"` 또는 `"openapi"` 키 존재).

### 6.8 컨테이너를 재시작했더니 검색/그래프가 비었다

정상이다(§3.1). 수렴 경로는 3개다.

1. **서버 재기동** — `Startup`이 `CheckDrift` → `RehydrateAll(ctx, false)`. 로그: `startup drift check episodic_detected=true episodic_reason="indexed docs 0 != manifest records 42" ...` → `startup rehydration complete episodes_indexed=42 nodes_upserted=... edges_upserted=... failures=0`.
2. **서버는 그대로 두고 요청** — stat-gate가 프로젝트 단위로 부분 재수화한다.
3. **강제** — `POST /v1/reindex` (`?verify=true`면 전량 해시 감사).

부분 실패는 숨기지 않는다: 실패 건마다 `startup rehydration failure detail=...` Warn이 따로 찍힌다. `startup: rehydrator not configured; derived stores unmanaged` 경고가 보이면 재수화가 아예 비활성 상태이고, `startup: drift check failed`가 보이면 hot 정본 자체를 못 읽은 것이다.

### 6.9 hot 디렉터리가 안 보인다

`hotstore.New`는 디스크를 건드리지 않는다("It does not touch the disk; directories are created lazily on first write"). `~/.local/dj-memory/`는 **첫 쓰기 때** 생성된다(디렉터리 0700, 파일 0600 — `internal/hotstore/file.go`). `{Home}/blobs` 캐시도 같은 규칙이다(`internal/blob/blob.go`). 서버만 띄우고 아무것도 저장하지 않았다면 없는 것이 정상이다.

### 6.10 블랙박스가 시작도 못 한다

| 로그 | 의미 |
|---|---|
| `prerequisite missing tool="docker version"` 등 | `docker version`, `docker compose version`, `aws --version`, `go version` 중 하나가 실패했다 (exit 2) |
| `cold store unreachable — blackbox cannot verify §10.4/6/8` | `head-bucket` 실패. 로그의 `fix` 필드에 생성 명령이 그대로 들어 있다 (exit 2). 이때 임시 home은 지우고 나간다 |
| `port 8420 already serving — killing stale listener for an idempotent re-run` | 정상. 하니스가 이전 실행의 좀비를 치운다 — `lsof -ti :8420`으로 PID를 받아 각각 `kill -9`를 **별도 프로세스로** 실행하고(셸 파이프가 아니다), `waitFor`로 "port 8420 released"를 최대 15초 기다린 뒤 진행한다 (`harness_test.go:302-307`) |

---

## 7. ⚠️ 설계 문서와 차이

1. **§8의 env 목록이 불완전하다.** 설계 문서 §8은 `DJ_MEMORY_HOME`, `DJ_MEMORY_USERNAME`, `DJ_MEMORY_S3_BUCKET`, `DJ_MEMORY_S3_REGION`, `AWS_PROFILE`, `DJ_MEMORY_OPENSEARCH_URL`, `DJ_MEMORY_NEO4J_URL` 7개만 나열한다. 코드에는 `DJ_MEMORY_LISTEN_ADDR`(§8에 없음, §7의 바인딩 규칙만 서술)과 `DJ_MEMORY_EPISODIC_TTL_DAYS`(§3.1에만 등장)가 추가로 있다. **본 문서 §4.1의 9개가 실제다.**
2. **`make LISTEN=...`은 서버에 전달되지 않는다.** Makefile의 `LISTEN`은 `start`의 안내 문구와 `make status`의 curl 대상에만 쓰이고, 서버는 `DJ_MEMORY_LISTEN_ADDR`을 읽는다. 설계 문서에는 이 이중 경로에 대한 언급이 없다. 포트를 바꾸려면 둘 다 설정해야 한다.
3. **`make run`과 `make start`.** 설계 문서 §8은 "서버 자체는 호스트에서 실행(`make run`)"이라고 쓰지만, 실제 진입점은 인프라 기동·헬스 대기를 포함한 `make start`다. `make run`은 인프라가 이미 떠 있다고 가정하는 개발 루프용 타깃이다.
4. **"볼륨 없음"은 compose 선언에만 해당한다.** 설계 §8·§11은 파생 저장소를 볼륨 없는 비영속으로 규정하지만, `neo4j` 이미지가 스스로 `VOLUME /data /logs`를 선언하므로 `up` 마다 익명 볼륨 2개가 실제로 생성된다. 데이터가 재기동마다 비는 것(설계 의도)은 그대로지만, 정리는 compose가 해주지 않는다 — `infra-down`과 블랙박스 `composeFresh`가 `down -v`를 쓰는 이유이고, 설계 문서에는 이 부작용에 대한 언급이 없다.

---

## 8. 코드 위치

| 개념 | 파일 | 앵커 |
|---|---|---|
| make 타깃 전체 | [`Makefile`](../../Makefile) | `start`, `infra-down`, `infra-wait`, `test`, `live`, `blackbox` |
| 컨테이너 정의·헬스체크·포트 | [`deploy/docker-compose.yml`](../../deploy/docker-compose.yml) | `services.opensearch`, `services.neo4j` |
| nori 플러그인 이미지 | [`deploy/opensearch/Dockerfile`](../../deploy/opensearch/Dockerfile) | `opensearch-plugin install --batch analysis-nori` |
| env 로딩·기본값 | [`internal/config/config.go`](../../internal/config/config.go) | `Load()`, `envOr()`, `resolveHome()`, `resolveUsername()`, `resolveTTLDays()` |
| env 검증(loopback 강제, TTL) | [`internal/config/config.go`](../../internal/config/config.go) | `Config.Validate()`, `isLoopback()` |
| 튜닝 불가 상수 | [`internal/config/config.go`](../../internal/config/config.go) | `DefaultListenAddr`, `DefaultS3Region`, `DefaultNeo4jUser`, `MaxProjectFileBytes`, `MaxDocumentChunks` |
| 부팅 순서·저하 경고·의존성 조립 | [`cmd/memory-mcp/main.go`](../../cmd/memory-mcp/main.go) | `main()`, `run()`, `build()` |
| 주입되는 시계 (`time.Now()`의 유일한 호출부) | [`internal/hotstore/clock.go`](../../internal/hotstore/clock.go) | `Clock`, `systemClock`, `NewSystemClock()` |
| 부팅 실패 로그의 구조 | [`internal/errs/errs.go`](../../internal/errs/errs.go) | `(*Error).LogValue()`, `Wrap()`, `Invalid()`, `Internal()` |
| Kind → HTTP 상태 매핑(503의 출처) | [`internal/server/apierr/apierr.go`](../../internal/server/apierr/apierr.go) | `From()`, `mappingFor()`, `publicMessage()` |
| 저하 어휘로 503을 만드는 곳 | [`internal/server/respond.go`](../../internal/server/respond.go) | `unavailable()`, `writeAPIError()` |
| 리스닝·그레이스풀 셧다운 | [`internal/server/server.go`](../../internal/server/server.go) | `ListenAndServe()`, `readHeaderTimeout`, `shutdownGrace` |
| 부팅 드리프트 체크·재수화 | [`internal/server/startup.go`](../../internal/server/startup.go) | `Startup()` |
| 저하 문자열 | [`internal/server/degraded.go`](../../internal/server/degraded.go) | `degradedSearch`, `degradedGraph`, `degradedCold`, `degradedPromotion` |
| `/v1/status` 리포트·S3 도달성 프로브 | [`internal/server/handlers_ops.go`](../../internal/server/handlers_ops.go) | `StatusReport`, `S3SyncStatus`, `handleStatus()`, `coldReachable()`, `driftReport()` |
| 드리프트 사유 문자열 | [`internal/rehydrate/rehydrate.go`](../../internal/rehydrate/rehydrate.go) | `reasonIndexNotConfigured` 외 |
| S3 클라이언트·리전 명시 | [`internal/cold/cold.go`](../../internal/cold/cold.go) | `New()`, `Config.validate()` |
| S3 키 레이아웃 | [`internal/cold/keys.go`](../../internal/cold/keys.go) | `EpisodeArchiveKey`, `KnowledgeSnapshotKey`, `BlobKey` |
| OpenSearch 인덱스명·매핑 | [`internal/search/mapping.go`](../../internal/search/mapping.go) | `IndexName`, `koreanAnalyzer`, `indexBody` |
| nori 매핑 자동 복구 | [`internal/search/index.go`](../../internal/search/index.go) | `ensureSchema()`, `ensureIndexLocked()`, `hasKoreanAnalyzer()` |
| Neo4j 드라이버 생성 | [`internal/graph/graph.go`](../../internal/graph/graph.go) | `New()`, `Close()` |
| hot 디렉터리 퍼미션 | [`internal/hotstore/file.go`](../../internal/hotstore/file.go) | `dirPerm`, `filePerm`, `writeFileAtomic()` |
| swag CLI 버전 고정 | [`tools.go`](../../tools.go) | build tag `tools` |
| live 테스트 게이트 | [`internal/search/live_test.go`](../../internal/search/live_test.go) · [`internal/graph/live_test.go`](../../internal/graph/live_test.go) · [`internal/cold/live_test.go`](../../internal/cold/live_test.go) | `liveEnvVar`, `liveEnvValue` |
| 블랙박스 env 주입·S3 프리플라이트 | [`test/blackbox/harness_test.go`](../../test/blackbox/harness_test.go) | `TestMain()`, `startServer()`, `composeFresh()` |
