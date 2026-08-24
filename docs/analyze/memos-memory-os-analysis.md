# [논문분석] MemOS: A Memory OS for AI System

| 항목 | 내용 |
|---|---|
| 논문 | [arXiv:2507.03724v4](https://arxiv.org/abs/2507.03724) (2025-07-04 최초, 2025-12-03 v4) |
| 저자/기관 | Zhiyu Li 외 38명 — MemTensor(상하이) 주도, 상하이교통대·저장대·인민대·북경대·통지대 등 |
| 프로젝트 | https://memos.openmem.net / 코드: https://github.com/MemTensor/MemOS |
| 분석일 | 2026-08-24 |
| 분석 목적 | memory-mcp 설계에 이식할 메모리 관리 구조 검토 |

## TL;DR

LLM의 메모리를 **OS가 관리하는 일급 시스템 자원**으로 격상시키자는 제안. RAG를 "라이프사이클·버전·권한 관리가 없는 stateless 임시방편"으로 규정하고, 그 대안으로 ① 3가지 메모리 타입(Plaintext/Activation/Parameter)의 계층화와 상호 전환, ② 표준 캡슐화 단위 MemCube, ③ OS식 3계층 아키텍처(Interface/Operation/Infrastructure)를 제시한다. LoCoMo·LongMemEval·PreFEval·PersonaMem 전 벤치마크에서 Mem0, Zep, MIRIX 등 기존 메모리 시스템을 제치고 종합 1위. 단, **knowledge update 서브태스크는 상대적 약점**(74.3 vs Memobase 89.7).

---

## 1. 문제의식: 왜 "메모리 OS"인가 (§1)

저자들이 꼽는 4가지 실패 시나리오:

1. **장거리 의존성** — 컨텍스트 윈도우 한계·어텐션 비용 때문에 긴 태스크에서 초반 지시(코드 스타일, 사용자 정의 규칙)가 잊히고 기본값으로 회귀한다.
2. **지식 진화** — 법·규정·과학 지식은 계속 바뀌는데, RAG는 버전·출처·시간 인식이 없어 "낡은 규정과 새 규정을 화해 없이 동시에 인용"한다. 낡은 사실을 은퇴시키는 메커니즘이 없다.
3. **개인화·다중 역할** — 세션마다 백지 리셋. 기존 제품 메모리(ChatGPT 등)도 용량 한계·불안정한 접근·불투명한 업데이트 문제가 있다.
4. **플랫폼 간 이동 불가** — "ChatGPT에서 탐구한 아이디어를 Cursor로 가져갈 수 없는" 메모리 섬(memory island) 문제.

**공통 원인 진단**: 개별 모듈의 결함이 아니라 *메모리를 조직·운영하는 시스템 수준 메커니즘의 부재*. 따라서 해법도 "캐시 추가"나 "검색 모듈 부착"이 아니라 OS 수준의 자원 관리 재설계여야 한다.

## 2. 설계 철학: Computer OS → Memory OS (§3)

전통 OS 구성요소를 메모리 세계에 1:1 매핑한다 (Table 2):

| 계층 | 전통 OS | MemOS | 역할 |
|---|---|---|---|
| Core Operation | 레지스터/마이크로코드 | Parameter Memory | 장기 능력 |
| | 캐시 | Activation Memory | 빠른 작업 상태 |
| | I/O 버퍼 | Plaintext Memory | 외부 에피소드 |
| Management | 스케줄러 | MemScheduler | 연산 우선순위 |
| | 파일시스템 | MemVault | 버전 저장소 |
| | 시스템 콜 | Memory API | 통합 접근 |
| | 디바이스 드라이버 | MemLoader/Dumper | 메모리 이동 |
| | 패키지 매니저 | MemStore | 번들 공유 |
| Governance | 인증/ACL | MemGovernance | 접근 제어 |
| | syslog | Audit Log | 감사 추적 |
| | 예외 처리기 | Error Recovery | 오류 복구 |

**3대 원칙** (§3.1):
- **Memory as a System Resource** — 플랫폼 간 메모리 섬 해체, 스케줄 가능한 일급 자원화
- **Evolution as a Core Capability** — 모델과 메모리의 공진화. Pre-training → Post-training 다음 스케일링 축으로 **"Mem-training"** 을 주장 (Figure 4)
- **Governance as the Foundation for Safety** — 접근 제어·버전·감사 없이는 신뢰 가능한 장기 에이전트 불가

## 3. 핵심 설계 (테마별 구조와 이유)

### 3.1 3가지 메모리 타입과 상호 전환 (§4.1)

| 타입 | 실체 | 강점 | 약점 |
|---|---|---|---|
| Plaintext | 검색되는 명시적 지식(문서·그래프·프롬프트 템플릿) | 편집·추적·독립 저장 가능 | 매번 검색·주입 비용 |
| Activation | KV-cache, hidden state, 어텐션 가중치 | 즉각 응답, 저지연 | 휘발성, 암묵적 |
| Parameter | 가중치에 새겨진 지식(LoRA 모듈 포함) | 효율·표현력 최고 | 업데이트 비용 큼, 해석 불가 |

**전환 경로** (Figure 5) — 이 논문의 가장 독특한 기여:

- `Plaintext → Activation`: 자주 쓰는 평문을 미리 KV 형태로 변환해 디코딩 가속 ("instant memory path")
- `Plaintext/Activation → Parameter`: 태스크를 가로질러 안정된 지식은 LoRA 증류로 "능력 플러그인"화
- `Parameter → Plaintext`: **낡은 파라미터 지식은 평문으로 강등(backpatch)** — 재학습 없는 지식 교정 경로

**이유**: 세 타입의 비용–유연성–안정성 트레이드오프가 정반대이므로, CPU의 레지스터–캐시–메모리 계층처럼 "핫한 것은 승격, 콜드한 것은 강등"으로 비용을 최적화. 실측: KV 주입 방식은 프롬프트 주입 대비 TTFT 최대 91.4% 감소, 출력은 동일 (Table 8, Qwen2.5-72B 장문 컨텍스트).

### 3.2 MemCube: 표준 캡슐화 단위 (§4.2)

모든 메모리 = `Memory Payload`(내용) + `Metadata Header`. 메타데이터는 3그룹:

| 그룹 | 필드 | 존재 이유 |
|---|---|---|
| Descriptive Identifiers | timestamp, origin signature(추론 추출/사용자 입력/외부 검색/파인튜닝), semantic type(사실/선호/프롬프트) | 대규모 통합 스케줄링에는 "의미적 지문"에 의한 정확한 식별이 선행돼야 함 |
| Governance Attributes | 접근 범위(read/write/share), TTL·감쇠 규칙, 우선순위, 민감도 태그·워터마크 | 다중 사용자·장기 실행 환경에서 모델의 기본 추론만으로는 거버넌스 불가 |
| Behavioral Usage Indicators | 접근 빈도·최근성, contextual fingerprint(검색용 의미 시그니처), **version chain**(수정 이력·유래 계보) | 정적 라벨이 아닌 런타임 지표가 있어야 hot/cold 판정과 타입 간 전환(value-driven scheduling)이 가능 |

### 3.3 3계층 아키텍처 (§5)

**Interface Layer** — 진입점과 의도 해석
- `MemReader`: 자연어 → 구조화된 `MemoryCall` (태스크 의도, 시간 범위, 토픽, 컨텍스트 앵커 추출)
- `Memory API`: Provenance API(출처 ID는 라이프사이클 전체 유지), Update API(**버전 인식** append/merge/overwrite, 스냅샷, 차등 쓰기), LogQuery API(접근 로그·실행 추적)
- `Memory Pipeline`: retrieve → augment → update → archive 같은 체인을 트랜잭션·롤백 지원 DSL로 정의
- *이유*: 메모리 조작이 자연어에 섞여 들어오므로, 시스템 콜처럼 표준화된 진입점에서 의도 추출·권한 검사를 먼저 해야 하위 계층이 결정론적으로 동작

**Operation Layer** — 제어 센터
- `MemOperator`: 태그(토픽·출처·신뢰도·감정) + 지식그래프 + 계층화(private/shared/global). 구조적(룰 기반)·의미적(벡터) 하이브리드 검색. `MemoryPathResolver`가 task–concept–fact 3계층 스키마로 "무엇을, 어디서, 어떤 순서로 찾을지" 결정
- `MemScheduler`: 태스크 의미·윈도우 크기·자원 제약으로 최적 메모리 타입 선택. 대화 연속성 태스크→KV 우선, 전문가 절차→파라미터 우선, 사실 조회→평문 우선. 타입 간 승격/강등도 담당
- `MemLifecycle`: 상태 기계 (아래 §4 참고)
- *이유*: 이 계층이 있어야 메모리가 "정적 데이터 조각이 아니라 동적·컨텍스트 인식 자원"이 됨

**Infrastructure Layer** — 저장·보안·유통
- `MemGovernance`: **3항 권한 모델(사용자 신원 × 메모리 객체 × 호출 컨텍스트)**, TTL 집행, 접근 빈도 기반 GC, 민감정보 자동 마스킹, 시맨틱 워터마킹
- `MemVault`: 네임스페이스별 저장소(사용자 사설/전문 지식/산업 공유/파이프라인 캐시), `MemoryAdapter`로 벡터·관계형·블롭 백엔드 추상화
- `MemLoader/Dumper`: 플랫폼 간 양방향 이동(내보내기 시 권한 메타데이터·마스킹 필드 포함)
- `MemStore`: publish–subscribe 메모리 유통. 라이선스·과금 조건부 접근(Memory-as-a-Service)
- *이유*: 컴플라이언스(의료·금융), 메모리 섬 해체, 에이전트 간 지식 거래 생태계

## 4. 지식 풍화·변경 관리 (본 분석의 핵심 관심사)

> 지식은 낡고(풍화), 바뀌고(갱신), 충돌한다. MemOS의 답: **삭제·덮어쓰기가 아니라 상태 전이 + 버전 관리**.

### 4.1 풍화(시간 축): 정책이 관리하는 단계적 강등

- **상태 기계** (§5.4.3): `Generated → Activated → Merged → Archived (→ Expired)`. 전이 트리거 = 시스템 휴리스틱(최근성·접근 빈도·컨텍스트 관련성·병합 이벤트) + 사용자 액션. 상태가 곧 스케줄링 우선순위·저장 계층을 결정(Activated=고속 캐시, Archived=콜드 스토리지)
- **TTL·GC** (§5.5.1): Lifespan Policy(TTL/감쇠 규칙)를 MemGovernance가 집행. 접근 빈도 기반 가비지 컬렉션, "usage heat" 추적
- **캐시 무효화에 contextual drift 반영** (§5.4.1): 빈도뿐 아니라 컨텍스트 표류를 휴리스틱에 포함
- 핵심: 완전 삭제 전에 **Archived라는 완충 단계**가 있어 복구 가능성을 보존

### 4.2 변경(내용 축): 비파괴적 갱신

- **버전 인식 Update API**: append/merge/overwrite 3모드, 스냅샷·라벨 기반 차등 쓰기. 사용자 정정(user correction)이 명시적 유스케이스. 업데이트가 상태 전이·인덱스 갱신을 연쇄 트리거
- **Merged 상태 = 의미적 통합**: 기존 메모리와 의미 중복 감지 시 별도 항목으로 쌓지 않고 새 버전으로 통합. Plaintext 수준에서 충돌 감지·중복 제거·망각 정책 명시 (§4.1)
- **신뢰 소스 우선 스케줄링** (§7.2.2, 임상 가이드라인 예시): 새 가이드라인이 MemStore로 배포 → "trusted source" 태그 → 구버전과 비교 후 업데이트 제안 → 추론 시 신뢰·활성 버전 우선, 구버전 자동 아카이브. **재학습 없이, 파국적 망각 없이** 최신성 유지. 개인 노하우는 공식 지식과 공존하며 태스크 컨텍스트로 선택(덮어쓰지 않음)

### 4.3 파라미터에 박힌 낡은 지식

가장 어려운 경우에 대한 답이 **Parameter → Plaintext backpatch**: 낡거나 일관성 깨진 파라미터 지식을 편집 가능한 평문으로 되돌려 내리고, 추론 시 평문 버전이 우선 주입되게 함.

### 4.4 복구와 감사

- **Time Machine** (§5.4.3): 상태 스냅샷·버전 롤백. 아카이브 복원으로 what-if 시뮬레이션 가능(정본·감사 추적은 불변)
- **Frozen 상태**: 법률 계약·표준 지침은 업데이트 봉인 + 수정 이력 전체 보존
- **Provenance ID + mutation log**: "이 답이 어느 버전의 어느 지식에서 나왔는지" 역추적

## 5. 평가 결과 (§6)

전 방법 동일 백본(GPT-4o-mini), H800 80GB 단일 GPU. 비교 대상: MIRIX, Mem0, Zep, Memobase, MemU, Supermemory.

| 벤치마크 | MemOS-1031 | 비고 |
|---|---|---|
| LoCoMo (LLM-judge overall) | **75.80** (1위) | single-hop 81.09, multi-hop 67.49 1위. temporal reasoning 75.18은 Memobase(81.20)에 밀림. F1 45.27도 Memobase(50.18)가 우위 |
| LongMemEval (overall) | **77.8** (1위) | single-session-user 95.7, preference 96.7 압도적. **knowledge update 74.3은 Memobase(89.7)에 밀림** — 유일하게 1–2위 밖 |
| PreFEval (Personalized Response) | **77.2** (0-turn) / **71.9** (10-turn) | Preference Unaware 오류율 4.6%/7.4%로 최저 |
| PersonaMem (precision 1-in-4) | **61.2** (1위) | |
| API 강건성 (Table 7) | 100 QPS까지 성공률 100% | 타 시스템은 40 QPS에서 성공률 붕괴(Mem0 51.2%, MemU 3.8%) |
| KV 가속 (Table 8) | TTFT 최대 91.4% 감소 | 출력 동일성 검증됨 |

### LongMemEval "knowledge update" 항목이란

LongMemEval(arXiv:2410.10813)의 6개 질문 유형 중 하나. **사용자 정보가 세션에 걸쳐 변경되는 시나리오**에서, 질문 시점에 낡은 값이 아니라 **최신 값으로 답하는지**를 측정한다(예: 초반 세션 "혼다 탄다" → 후반 세션 "테슬라로 바꿨다" → "내 차 뭐지?"에 테슬라로 답해야 정답). 순진한 검색 기반 메모리는 두 사실을 모두 회수해 낡은 값을 답하기 쉽다 — 바로 이 논문이 §1에서 RAG의 결함으로 지적한 "낡은 사실을 은퇴시키지 못하는 문제"의 직접 측정치다.

## 6. 한계와 비판적 독해

1. **거버넌스 골격 ≠ 판정 완성도**: 버전·상태·신뢰 태그라는 *구조*는 있지만, "언제 병합하고 언제 은퇴시킬지"의 *판정*은 휴리스틱(빈도·최근성·의미 중복) 의존. 그 결과가 knowledge update 74.3 vs Memobase 89.7의 격차로 드러남. 자기네 핵심 세일즈 포인트(지식 진화 관리)의 직접 측정치에서 밀린다는 점이 아이러니
2. 저자들 스스로 §1에서 메모리 관리가 궁극적으로 "model-defined"(모델이 학습한 전략)여야 한다고 못 박음 — 즉 현재 버전은 hard-coded 정책 단계이며, 판정 학습은 미래 과제
3. Activation/Parameter 전환의 실효성은 KV 가속(Table 8) 외에는 종단 벤치마크 기여도가 분리 검증되지 않음 — 종합 점수는 사실상 Plaintext 계층(하이브리드 검색 + 스케줄링)의 성과일 가능성
4. Mem-training 스케일링 주장(Figure 4)은 비전 선언에 가깝고 실증 없음
5. 39명 저자·자사(MemTensor) 주도 벤치마크라는 점은 감안 필요. 단, 동일 백본·공개 코드로 재현 가능성은 열어둠

## 7. memory-mcp 이식 포인트

벤치마크 성적과 무관하게 **구조적으로 검증된** 세 가지:

### ① 5단계 상태 + Archived 완충층

메모리를 즉시 삭제하지 않고 상태로 관리:

```
Generated → Activated → Merged → Archived → Expired
    (생성)     (참조됨)    (통합됨)   (콜드 보관)   (만료)
```

- 전이 트리거: 접근 빈도·최근성(시스템) + 명시적 사용자 액션
- Archived는 검색 기본 대상에서 빠지되 복원 가능 — "잘못 잊음"에 대한 보험
- 메타데이터 최소 필드: `state`, `created`, `last_used`, `usage_count`

### ② 버전 체인 + 비파괴 병합

- 갱신 시 파괴적 덮어쓰기 금지. 새 버전 생성 + `supersedes`/`superseded_by` 링크로 체인 구성
- 의미 중복 감지 시 별도 항목 누적이 아니라 **병합된 새 버전**(Merged) 생성
- 충돌하는 두 사실이 공존할 때: 둘 다 보존하되 활성 버전 하나만 검색 우선순위 부여
- 효과: 롤백 가능, "왜 이렇게 기억하고 있지?"의 유래 추적 가능

### ③ 신뢰 소스 태그 × 검색 우선순위 연동

- `origin`(사용자 직접 입력/대화 추론/외부 문서)과 `trust` 수준을 메타데이터로 분리
- 검색(recall) 시 신뢰·활성 버전을 우선 노출, obsolete 버전은 명시 요청 시에만
- 사용자 정정(correction)은 최고 신뢰 등급 — 기존 추론 기반 메모리를 자동으로 아래로 밀어냄

### 참고할 만한 부가 요소

- **Provenance ID**: 생성부터 만료까지 유지되는 불변 출처 식별자 — 디버깅("이 답 어디서 나왔어?")에 직결
- **3항 권한 모델**: 사용자 × 메모리 × 호출 컨텍스트 — 다중 프로젝트/에이전트 공유 시점에 필요
- **usage heat 기반 hot/cold**: 인덱스(MEMORY.md류)에 올릴 것과 온디맨드 검색으로 내릴 것의 판정 기준

## 8. 논문의 향후 과제 (§8)

- **Cross-LLM Memory Sharing**: 모델 간 파라미터·활성화 메모리 공유를 위한 Memory Interchange Protocol(MIP) 표준화
- **Self-Evolving MemBlocks**: 사용 피드백으로 자기 최적화·재구성하는 메모리 단위 (수동 유지보수 제거)
- **Scalable Memory Marketplace**: 자산 수준 거래·협업 업데이트·분산 진화를 지원하는 탈중앙 메모리 유통

## 관련 링크

- 논문: https://arxiv.org/abs/2507.03724
- 코드: https://github.com/MemTensor/MemOS
- 프로젝트: https://memos.openmem.net
- 선행 연구 Memory³ (명시적 메모리 계층 이론): JML 2024, 논문 ref [1]
- LongMemEval (knowledge update 벤치마크): https://arxiv.org/abs/2410.10813 / https://github.com/xiaowu0162/LongMemEval
