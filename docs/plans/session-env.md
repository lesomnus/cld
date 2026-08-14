# 세션 환경 설정 — 구현 계획

cld.yaml에서 **claude 세션의 실행 환경**(환경변수, 파일, 셋업 스크립트)을 선언할 수
있게 하는 기능의 설계와 단계별 구현 계획. 진행 상황은 [단계](#단계)의 체크박스로
갱신한다.

## 왜

컨테이너 이미지나 `devcontainer.json`을 고치지 않고, 내 머신의 설정만으로 claude가
보는 환경을 바꾸고 싶다. 대표 시나리오는 **원격 도커 엔진과 그 자격증명을 특정
프로젝트의 claude에게만 넘기는 것**(`DOCKER_HOST` + TLS 인증서)이고, 그 밖에
프로젝트별 `AWS_PROFILE`, 개인 취향 `EDITOR`/`PATH`, 이미지에 없는 도구 설치가 같은
틀에 들어온다.

## 주입 지점: 컨테이너 생성이 아니라 데몬의 exec

**컨테이너 생성 시점 주입은 하지 않는다.** 이유:

- cld는 컨테이너를 만들지 않는다. `cld up`은 devcontainer CLI에 위임하고, 데몬은
  이미 뜬 컨테이너를 docker 이벤트로 발견해 프로비저닝한다. VS Code가 띄운
  컨테이너에는 생성 시점 주입이 애초에 불가능하고, 컨테이너 출처에 따라 동작이
  달라지는 건 최악이다.
- `devcontainer up --remote-env`는 CLI가 돌리는 라이프사이클 명령에만 적용되고
  컨테이너 설정에 남지 않는다. 나중에 데몬이 거는 `docker exec`은 그 값을 못 본다.
  진짜로 남기려면 남의 레포의 `devcontainer.json`을 고쳐야 하는데 선을 넘는다.
- 생성 시점에 구우면 cld.yaml을 고쳐도 컨테이너 재생성 전까지 반영되지 않는다.

그래서 주입 지점은 **`daemon.session_env`** 하나다. 컨테이너를 누가 띄웠든 claude
pane, split pane, 그리고 이 계획이 추가하는 스크립트는 전부 데몬의 exec을 거치므로
동작이 대칭이 된다.

**경계(문서에 명시할 것):** cld가 넣은 env는 cld가 띄우는 프로세스에만 보인다. VS
Code의 일반 터미널, `postCreateCommand`, 사용자가 직접 친 `docker exec`에는 보이지
않는다(cld가 심는 claude 터미널 프로파일은 같은 세션에 붙으므로 보인다). 자격증명
용도에서는 이 격리가 오히려 장점이다 — 컨테이너 `Config.Env`에 박히지 않아
`docker inspect`로 새지 않는다.

컨테이너 전체에 걸려야 하는 env, 포트, 마운트는 계속 `devcontainer.json`의 몫이다.

## 형식

```yaml
# 전역: cld가 프로비저닝하는 모든 컨테이너에 적용
env:
  EDITOR: vim
  PATH: ${PATH}:/opt/mytools/bin      # 기존 값 확장
  GOFLAGS: ${GOFLAGS:--mod=mod}       # 없을 때만 기본값
  LESS: null                          # 이미지가 넣은 값을 제거

files:
  - src: ~/.config/mytool/            # 호스트 홈 아래여야 함
    dst: ${HOME}/.config/mytool
    mode: "0600"

scripts:
  setup: |                            # 컨테이너당 1회 (본문이 바뀌면 재실행)
    sudo apt-get update && sudo apt-get install -y ripgrep
  start: echo "generation ${CLD_STARTED_AT}"   # 컨테이너 기동 세대마다

# 프로젝트별: 매칭되는 항목이 파일 순서대로 누적 적용
projects:
  - match: ~/work/acme/**             # 문자열 또는 리스트
    env:
      AWS_PROFILE: acme
      DOCKER_HOST: tcp://build01.internal:2376
      DOCKER_TLS_VERIFY: "1"
      DOCKER_CERT_PATH: ${HOME}/.docker-remote
    files:
      - src: ~/.docker/build01/
        dst: ${HOME}/.docker-remote
    scripts:
      setup:
        run: [make, dev-setup]        # 배열이면 셸 없이 exec, 문자열이면 sh -c
        user: root                    # 기본은 컨테이너 사용자(claude와 동일)
        workdir: ${CLD_WORKSPACE}
        timeout: 10m                  # 기본 5m
        on_error: fail                # fail | warn(기본)
```

`hooks`가 아니라 `scripts`인 이유: Claude Code settings.json의 `hooks`(cld가 이미
UserPromptSubmit/Stop을 심는다)와 이름이 겹치면 혼란스럽다.

`match`는 `ignore:`가 쓰는 것과 같은 글롭이다(`devc.MatchPath` — `**`는 경로
구분자를 넘고, 앞의 `~/`는 홈으로 확장). 매칭되는 항목이 **여러 개면 전부**, 파일
순서대로 적용된다(first-match가 아니다).

## env 규칙

### 우선순위 (낮음 → 높음)

| # | 출처 | 비고 |
| --- | --- | --- |
| 1 | 이미지 `ENV` + `containerEnv` | 컨테이너 자체 env, exec이 상속한다 |
| 2 | `devcontainer.json`의 `remoteEnv` | 이 계획에서 새로 채택 |
| 3 | cld 소프트 기본값 | `TERM`, `LANG`, `DISABLE_AUTOUPDATER` — 덮어써도 된다 |
| 4 | cld.yaml 전역 `env` | |
| 5 | 매칭된 `projects[*].env` | 파일 순서대로, 뒤가 이긴다 |
| 6 | cld 관리 키 | 아래 예약 키. 항상 마지막 |

**devcontainer.json과 이름이 겹치면 cld.yaml이 이긴다.** devcontainer.json은 레포에
커밋되는 팀 공용 계약이고 cld.yaml은 내 머신의 개인 설정이라 더 구체적인 의도다.
덮어쓰고 싶지 않은 경우는 값 문법으로 표현한다.

### 값 문법

- `FOO: bar` — 무조건 설정
- `FOO: ${FOO}:/extra` — 기존 값 확장
- `FOO: ${FOO:-bar}` — 기존 값이 있으면 그대로, 없을 때만 설정
- `FOO: null` — 상속된 값을 제거(unset)
- `${env:NAME}` — **데몬 프로세스**의 env. 시크릿을 cld.yaml에 적지 않고
  `docker-compose.yaml`의 `environment:`나 `cld install`에서 주입하기 위한 소스
- `$$` — 리터럴 `$`

확장은 **이전 레이어만** 참조한다. 같은 맵 안의 형제 키는 참조할 수 없다 — 이러면
YAML 맵 순서에 의존하지 않아 해석이 결정적이다. 미정의 변수는 셸처럼 빈 문자열.

`devcontainer.json`에서 온 값은 그 파일의 문법을 따른다: `${containerEnv:NAME}`은
확장하고, `${localEnv:NAME}`은 **빈 문자열 + 경고**다 — 컨테이너화된 데몬은 호스트
사용자의 env를 볼 수 없다. 못 하는 것은 못 한다고 하는 편이 낫다.

### 예약 키

`CLAUDE_CONFIG_DIR`, `SSH_AUTH_SOCK`, `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`,
`GIT_CONFIG_GLOBAL`, `CLAUDE_CODE_ENABLE_TELEMETRY`, `OTEL_*` 는 사용자 env에서
**설정 로드 시 에러로 거절**한다. 프록시 모드에서 `ANTHROPIC_BASE_URL`이 덮이면
인증이 조용히 깨지는데, 조용한 오작동보다 로드 실패가 낫다. 런타임 상태(프록시
on/off)와 무관하게 항상 거절한다 — 설정의 유효성이 데몬 상태에 따라 달라지면 진단이
불가능해진다.

반대로 소프트 기본값(`TERM`, `LANG`, `DISABLE_AUTOUPDATER`, `ENABLE_TOOL_SEARCH`)은
취향 영역이라 덮어쓰기를 허용한다. 기준은 "cld가 다른 데서 한 약속을 지키는 데 그
값이 필요한가"다.

### 적용 범위

claude pane, split pane, 사용자 스크립트 — 즉 **사용자를 위해 띄우는 프로세스**만.
`resolve()`의 uid 프로브, `sha256sum` 체크, `ls` 같은 cld 내부 exec은 제외한다.
사용자가 `LANG`이나 `PATH`를 바꿔도 프로비저닝 파싱이 깨지지 않아야 한다.

### 반영 시점

env는 세션 생성 시 exec에 박히므로 **이미 떠 있는 세션은 바뀌지 않는다.**
`cld it --new` 또는 컨테이너 재시작이 필요하다. 데몬은 기동 시 config를 한 번만
읽으므로 데몬 재시작도 필요하다. 자동 리로드는 이 계획의 범위 밖(→ [추후](#추후)).

## files 규칙

- `src`는 **호스트 홈 아래**여야 한다. 컨테이너화된 데몬이 볼 수 있는 것은
  `cld install`이 붙이는 읽기 전용 홈 마운트(`/host-home`)뿐이다. 그 밖의 경로는
  로드 시 에러.
- `dst`는 컨테이너 경로. `${HOME}`, `${CLD_WORKSPACE}`를 확장한다.
- 소유자는 컨테이너 사용자(`uid`/`gid`), 모드 기본값은 파일 `0600` / 디렉터리
  `0700` — 자격증명이 주 용도이므로 넉넉하게 열지 않는다.
- 디렉터리면 트리째 복사한다(`dockerx.CopyDirToContainer`, 일반 파일과 디렉터리만).
- **매 reconcile마다 내용 해시를 비교해 갱신**한다. 자격증명은 도는 값이라
  "1회만"이면 곤란하다. 해시가 같으면 아무것도 하지 않는다.
- 배치 시점은 dotfiles 다음, 세션 생성 전.

## scripts 규칙

| 훅 | 주기 | 재실행 조건 |
| --- | --- | --- |
| `setup` | 컨테이너당 1회 | 컨테이너 안 마커(`$XDG_CACHE_HOME/cld/scripts/setup.sha256`)에 스펙 해시를 기록. 해시가 바뀌면 재실행 |
| `start` | 기동 세대마다 | `e.started_at` 키잉, 항상 실행 |

마커를 컨테이너 안에 두는 이유: 데몬을 재시작해도 중복 실행되지 않고, 컨테이너가
재생성되면 마커도 함께 사라져 자연히 다시 돈다.

- 실행 위치는 `ensure_` 안에서 dotfiles/vscode 다음, 세션 생성 전. 스크립트가 깐
  도구를 claude가 바로 쓸 수 있어야 한다.
- 사용자 기본값은 `e.user`(claude와 동일), `user: root`로 승격 가능. CWD 기본값은
  워크스페이스.
- env는 해석된 세션 env + `CLD_EVENT`, `CLD_CONTAINER_ID`, `CLD_WORKSPACE`,
  `CLD_NAME`, `CLD_STARTED_AT`.
- 문자열이면 `sh -c`, 배열이면 셸 없이 argv 그대로.
- 전역과 매칭된 프로젝트 스크립트는 **이어붙여** 순차 실행한다(교체가 아니다).
- `on_error: warn`(기본)이면 로그만 남기고 프로비저닝을 계속한다. 개인 스크립트가
  깨졌다고 claude 세션에 못 들어가는 것이 최악이다. `fail`이면 `StatusFailed`.
- **타임아웃 필수**(기본 5m). entry의 워커 고루틴은 직렬이라 스크립트가 매달리면 그
  컨테이너의 모든 reconcile이 멈춘다.

## 진단

`cld setting env <name>` — 컨테이너의 실효 세션 env를 키마다 출처와 함께 출력한다.
이게 없으면 "왜 내 env가 안 먹지"를 추적할 방법이 없다.

## 알아둘 이름 충돌

`ensure.go`는 tmux pane 명령 앞에 이미 `DOCKER_HOST=`를 붙인다. 그것은 **호스트 쪽
`cld x exec` 클라이언트**가 devcontainer를 띄운 엔진에 붙기 위한 값이고, 이 계획의
`DOCKER_HOST`는 **컨테이너 안 claude**를 위한 값이다. 주입 지점이 명령줄 prefix와
exec env로 분리되어 실제 충돌은 없지만(사용자 env가 pane 클라이언트를 망가뜨릴 수
없다), 같은 이름이 두 층에서 다른 뜻이라는 것은 문서에 못 박는다.

## 단계

- [x] 1. `internal/envx` — 레이어 병합, `${}` 확장, provenance. 순수 함수, 단위 테스트
- [x] 2. 설정 스키마 — `cmd/config`에 `env`/`files`/`scripts`/`projects`와 검증
      (`cmd/config/session.go`). `src`는 `~/` 시작을 강제한다 — 호스트 데몬과
      컨테이너 데몬이 절대 경로를 다르게 해석하는 모호함을 없앤다.
- [x] 3. `session_env` 재구성 — 컨테이너 env 캡처, 사용자 레이어 배선, unset 래퍼.
      `env_managed`의 모든 키가 예약되어 있는지 `TestSessionEnvManagedKeysAreReserved`가
      감시한다(관리 레이어는 조용히 이기므로, 예약되지 않은 키가 있으면 사용자는
      설정이 무시되는 이유를 알 방법이 없다). 그래서 `ENABLE_TOOL_SEARCH`도 예약에 추가.
- [x] 4. `devc.RemoteEnv` — metadata 라벨 + config 파일에서 remoteEnv 채택
- [ ] 5. `files` 배치 — 해시 비교 갱신
- [ ] 6. `scripts` — setup/start, 마커, 타임아웃, on_error
- [ ] 7. `cld setting env` — 데몬 엔드포인트 + 클라이언트 출력
- [ ] 8. 사용자 문서 — `docs/session-env.md`, README, cld.yaml 주석

1~4는 도커 없이 검증된다(`internal/daemon`의 기존 env 테스트가 `session_env`를 직접
호출한다). 5~7은 그 위에 얹힌다.

### unset 구현 메모

docker exec의 `Env`는 추가/치환만 가능하고 상속된 env를 지울 수 없다. `null`로
지정된 키가 있으면 원격 argv를 감싼다:

```
sh -c 'unset FOO BAR; exec "$@"' sh <argv...>
```

`"$@"` 형태라 argv를 문자열로 재인용할 필요가 없고, `env -u`와 달리 별도 바이너리에
의존하지 않는다(sh는 이미 `session_command`가 전제한다).

## 범위 밖

- 워크스페이스 안의 프로젝트 파일(`.devcontainer/cld.yaml`) 읽기. 레포에 쓰기
  권한이 있는 사람이 세션에 env와 셸 스크립트를 주입할 수 있고, 그 용도라면 보통
  `devcontainer.json`에 쓰면 된다.
- 컨테이너 전체에 걸리는 env, 포트, 마운트 — `devcontainer.json`의 몫.
- 컨테이너에 docker CLI 주입 — devcontainer feature의 몫.

## 추후

- cld.yaml 자동 리로드(mtime 감시 또는 SIGHUP). env/스크립트는 dotfiles보다 훨씬
  자주 손대는 설정이라 "고치고 데몬 재시작"이 거슬릴 수 있다.
- `pre_attach` 훅.
