# cld — devcontainer용 Claude Code 런처 데몬

## 목표

docker event를 listen해서 devcontainer가 뜨면, 호스트에 캐시해둔 claude code CLI를 컨테이너 내부에 복사하고, **호스트에서 tmux 세션**을 만들어 `docker exec`로 컨테이너 안에서 claude를 실행해주는 데몬.

핵심 요구사항: **claude는 항상 컨테이너 안에서 실행된다(샌드박스).** 컨테이너에 넣는 것은 claude 바이너리와 (기록 동기화 watcher로 재사용하는) cld 자신뿐이고, tmux를 포함해 다른 어떤 것도 컨테이너에 설치하지 않는다.

보안 경화(소켓 권한, 캐시 변조 등)는 범위 밖 — 단일 사용자 호스트를 가정하고, 호스트가 침해되지 않는 것은 사용자 책임.

## 확정된 설계 결정

- **기본 경로는 `~/.cache/cld`** (`$XDG_CACHE_HOME` 존중, config로 변경 가능). 바이너리 캐시 `bin/`, API 소켓 `cld.sock`, tmux 서버 소켓 `tmux.sock`이 전부 이 아래. `/tmp`는 tmpfs라 캐시가 RAM을 먹고 재부팅에 날아가므로 쓰지 않는다.
- **docker 연동은 SDK(`github.com/docker/docker/client`)를 쓴다.** 이벤트 스트림, cp(CopyToContainer), exec attach를 전부 API로 직접 다루고, CLI 셸아웃은 하지 않는다. `DOCKER_HOST`를 그대로 존중하므로 원격/사이드카 엔진에도 동작하고, 호스트에 docker CLI가 없어도 된다.
- **tmux는 호스트에서 실행.** 세션 하나당 pane 커맨드로 `cld`의 내부 서브커맨드(exec attach 클라이언트)를 실행한다 — SDK로 exec create/attach + raw TTY + SIGWINCH→resize를 처리하는 `docker exec -it -u <user> -w <workspace> <ctr> claude`의 cld 구현. devcontainer 기본 이미지에는 tmux가 없으므로 컨테이너 안 tmux는 애초에 성립하지 않고, 호스트 tmux면 컨테이너에 tmux를 넣지 않아도 된다.
  - 데몬 전용 tmux 서버 소켓을 쓴다: `tmux -S ~/.cache/cld/tmux.sock`. 사용자의 기본 tmux 서버와 섞이지 않는다.
  - 세션 이름은 `cld-<이름>` (tmux target 문법과 충돌하는 `.` `:` 는 치환).
  - 컨테이너가 멈추거나 claude가 종료되면 exec이 끝나고, pane의 cld가 데몬에 통지(최종 기록 동기화 + 상태 갱신 트리거)한 뒤 pane과 함께 세션이 닫힌다. 다음 start 이벤트에서 새로 만든다. (스크롤백 유지가 필요해지면 `remain-on-exit` + `respawn-pane`으로 확장 — v1은 단순하게.)
- **`it`은 데몬을 거치지 않는다.** 이름 → 세션 이름이 규칙으로 정해지므로 클라이언트가 그냥 호스트 tmux에 attach 한다.
- **HTTP API(`~/.cache/cld/cld.sock`)는 control-plane 전용.** `ls`(목록·상태 조회)만 처리한다. TTY는 절대 이 소켓으로 흘리지 않는다.
- **컨테이너 FS 조작은 `docker cp` 한 가지.** 이미 실행 중인 컨테이너에 호스트 디렉터리를 마운트로 추가하는 건 Docker가 지원하지 않는다(마운트는 생성 시점에 고정). 우회는 아래 "사후 마운트에 대해" 참고 — v1은 복사로 간다.

## devcontainer 감지

- devcontainer CLI / VS Code가 컨테이너에 다는 라벨로 감지한다:
  - `devcontainer.local_folder` — 호스트 쪽 워크스페이스 경로. **이 라벨의 존재 여부가 감지 조건.**
  - `devcontainer.config_file` — devcontainer.json의 호스트 경로.
- **compose는 자동으로 해결된다.** devcontainer CLI가 `service`로 지정된 주 서비스에만 위 라벨을 붙이므로, 라벨 필터만으로 "워크스페이스가 있는 컨테이너만" 걸러진다. 별도 compose 처리 불필요.
- **표시 이름 = `basename(devcontainer.local_folder)`.** 컨테이너 이름 파싱(`_devcontainer` 접미사 제거)은 쓰지 않는다 — non-compose devcontainer는 랜덤 이름이고 compose도 프로젝트명이 가변이라 신뢰할 수 없다. 이름이 충돌하면 짧은 컨테이너 ID를 붙여 구분.
- **컨테이너 내부 워크스페이스 경로**: 라벨에는 없다. `docker inspect`의 `.Mounts`에서 `Source == devcontainer.local_folder` 인 항목의 `Destination`을 쓰고, `devcontainer.config_file`의 devcontainer.json에 `workspaceFolder`가 명시돼 있으면 그걸 우선한다.
- **실행 유저**: `devcontainer.metadata` 라벨(이미지에서 상속)의 `remoteUser` → 없으면 `.Config.User` → 없으면 uid 1000. 이 유저로 exec 해야 파일 소유권·$HOME·claude 설정 위치가 맞는다.

## 특정 devcontainer 무시 (opt-out)

두 가지 방법, 라벨이 우선:

1. **라벨 `cld.ignore=true`** — 프로젝트 쪽에서 선언. non-compose는 devcontainer.json에 `"runArgs": ["--label", "cld.ignore=true"]`, compose는 해당 서비스에 `labels: ["cld.ignore=true"]`.
2. **config의 ignore 글롭 목록** — cld.yaml에 `ignore: ["~/work/vendor/**"]` 처럼 `devcontainer.local_folder` 경로에 매칭. 프로젝트 쪽을 못 고치거나 디렉터리 단위로 일괄 제외할 때.

무시된 컨테이너는 provision 대상에서 빠지고 `ls`에도 나오지 않는다.

## serve

데몬 본체. 시작 순서가 중요하다:

1. **이벤트 구독을 먼저 연다** (`start`, `die`, `destroy` 필터).
2. 그 다음 컨테이너 목록을 순회하며 reconcile한다. 구독→목록 순서라 그 사이에 뜬 컨테이너를 놓치지 않는다 (ensure가 멱등이므로 중복 이벤트는 무해).
3. 이후 이벤트 처리:
   - `start` → `ensure(컨테이너)`. (`create`는 무시 — 아직 안 돌고 있어서 exec이 전부 실패한다.)
   - `die` / `destroy` → 목록에서 제거, 남은 tmux 세션 정리.
   - 이벤트 스트림이 끊기면 재구독 후 다시 reconcile.

`ensure(컨테이너)` — 멱등, 매번 안전하게 재실행 가능:

1. 컨테이너 플랫폼 판별: arch는 inspect, libc는 musl 프로브(`/lib/libc.musl-*.so.1` 존재 확인). Alpine은 `libgcc`/`libstdc++`가 없으면 claude가 안 도니, 없으면 로그 남기고 skip.
2. 해당 플랫폼 바이너리가 캐시에 없으면 다운로드 (동시 요청은 singleflight로 1회만).
3. 컨테이너 안에 바이너리가 없거나 구버전이면 `docker cp`로 `/usr/local/bin/claude-<version>` 넣고 symlink `/usr/local/bin/claude` 원자 교체. (실행 중인 바이너리를 덮어쓰면 ETXTBSY로 실패하므로 반드시 버전 경로 + symlink.)
4. `.claude.json`·`settings.json` 시드와 대화 기록 복원 (아래 "인증과 첫 실행", "대화 기록 지속" 참고).
5. 호스트 tmux 세션이 없으면 생성. **단, 세션 생성은 (컨테이너 ID, start 이벤트)당 최대 1회.** 사용자가 claude를 `/exit` 하거나 세션을 죽였으면 그건 의도이므로 데몬이 되살리지 않는다. 컨테이너가 재시작되면 새 start 이벤트에서 다시 만든다.
   - 세션 env: `CLAUDE_CONFIG_DIR=<home>/.claude`, `DISABLE_AUTOUPDATER=1`, `TERM=xterm-256color`.
   - 세션 커맨드: 해당 프로젝트에 기존 대화가 있으면(`projects/<인코딩된 경로>/*.jsonl` 글롭) `claude --continue`, 없으면 `claude`. (`--continue`는 기록이 없으면 "No conversation found"로 실패하므로 무조건 붙이면 안 된다.)
6. 기록 동기화 대상이면 watcher exec이 살아있는지 확인, 없으면 시작 (아래 "대화 기록 지속").

동시성: 이벤트 처리는 컨테이너 ID별로 직렬화하고, 서로 다른 컨테이너는 병렬로 처리한다 (전역 직렬 큐는 다운로드 하나가 전체를 막는 head-of-line blocking이 생김). 모든 docker 호출에 timeout을 건다. 상태 목록은 메모리 캐시일 뿐이고, 진실은 항상 컨테이너 프로브(바이너리 존재 + `tmux has-session`)다 — 데몬 재시작 시 reconcile로 복구된다.

## 바이너리 캐시와 버전 관리

- 캐시 경로: `~/.cache/cld/bin/<version>/<platform>/claude` — 재부팅에도 유지된다.
- 다운로드 채널: install.sh를 셸아웃하지 않고 그 뒤의 HTTP API를 직접 쓴다 — `https://downloads.claude.ai/claude-code-releases`
  - `GET /stable` → 버전 문자열
  - `GET /{version}/manifest.json` → 플랫폼별 sha256
  - `GET /{version}/{platform}/claude` → 바이너리 (linux-x64, linux-arm64, linux-x64-musl, linux-arm64-musl)
- 최신 버전 확인은 데몬 시작 시 + 주기적으로. **다운로드 → sha256 검증 → rename, 그 후에** 구버전 캐시 삭제 (delete-first 금지 — 다운로드 실패 시 캐시가 비면 안 된다). 네트워크가 안 되면 캐시된 버전으로 동작.
- 실행 중인 세션은 절대 건드리지 않는다. 새 세션/새 컨테이너부터 새 버전.
- 컨테이너 안의 claude 자동 업데이트는 끈다 (바이너리는 cld가 관리하므로) — 세션 환경변수 `DISABLE_AUTOUPDATER=1` 주입 (수동 `claude update`까지 막으려면 `DISABLE_UPDATES=1`).

## 인증과 첫 실행

- **`.claude.json` 시드는 v1에 포함.** provision 시 `hasCompletedOnboarding`(+테마)과 프로젝트(워크스페이스 루트)별 `hasTrustDialogAccepted`·`hasCompletedProjectOnboarding`을 주입해 온보딩·신뢰 프롬프트를 건너뛴다. 내부 포맷이라 버전에 따라 깨질 수 있지만 편리함이 더 크다 — 깨져봤자 프롬프트가 다시 보일 뿐.
  - 세션에 `CLAUDE_CONFIG_DIR=<home>/.claude`를 설정하므로 시드 위치는 `<home>/.claude/.claude.json` (기본값인 `$HOME/.claude.json`이 아님 — 아래 "대화 기록 지속" 참고).
  - 기존 파일이 있으면 덮어쓰지 않고 JSON 병합(필요한 키만 추가). 파일 소유자는 remoteUser.
  - `settings.json`에는 `cleanupPeriodDays: 365`를 시드 (기본 30일, mtime 기준 시작 시 삭제 — 복원된 옛 대화가 지워지는 것 방지).
- 로그인(자격증명)은 시드로 해결되지 않는다. **최초 1회만** 사용자가 첫 `cld it`에서 로그인하면 `.credentials.json`이 global 백업으로 복사-아웃되고, 이후 모든 컨테이너·프로젝트에 복원된다(아래 "대화 기록 지속"). `~/.claude`를 마운트하는 기존 구성에서도 동작하지만, cld가 자리잡으면 마운트는 없앨 예정. 추후 옵션: `claude setup-token`으로 만든 `CLAUDE_CODE_OAUTH_TOKEN`을 세션 환경에 주입.

## 대화 기록 지속 (컨테이너 재생성 대응)

사후 마운트가 불가능하므로 `docker cp` 스냅샷으로 대화 기록을 컨테이너 밖에 보존하고, 같은 프로젝트의 새 컨테이너에 복원한다.

전제가 되는 사실 (검증됨):

- 트랜스크립트는 `<config dir>/projects/<인코딩된 cwd>/<session-id>.jsonl` + 세션별 하위 디렉터리(subagents, tool-results). 인코딩은 cwd의 **비영숫자 문자 전부** `-` 치환 (`/workspace` → `-workspace`). 별도 인덱스 없이 디렉터리 스캔이라, 파일만 제자리에 있으면 `--continue`/`--resume`이 찾는다.
- `.claude.json`(온보딩·신뢰·프로젝트 상태)의 기본 위치는 `~/.claude` **바깥**(`$HOME/.claude.json`)이다. `CLAUDE_CONFIG_DIR`을 설정하면 그 디렉터리 안으로 들어온다.
- claude는 시작할 때 `cleanupPeriodDays`(기본 30일) 기준 **mtime**으로 오래된 세션 파일을 삭제한다. `docker cp`는 mtime을 보존하므로, 복원한 옛 대화가 첫 실행에서 지워질 수 있다 → 시드하는 settings.json에서 크게 잡는다(365).

설계:

- **세션마다 `CLAUDE_CONFIG_DIR=<home>/.claude`를 설정**해 `.claude.json`까지 포함한 모든 상태를 디렉터리 하나로 모은다. 백업·복원·마운트의 단위가 `<home>/.claude` 하나가 된다.
  - 한계: 이 env는 cld가 만든 tmux 세션에만 적용된다. 사용자가 컨테이너 셸에서 claude를 직접 실행하면 기본 위치를 본다.
- 컨테이너의 `<home>/.claude`가 **호스트 bind mount면 동기화하지 않는다**(마운트가 이미 지속성을 제공). `.Mounts`로 판별. cld가 자리잡으면 `~/.claude` 마운트를 없앨 예정이므로 **동기화가 기본 경로**이고, 마운트 감지는 과도기 호환용이다.
- **복사-아웃은 이벤트 드리븐** — 주기 폴링 대신:
  - cld가 자기 실행 파일(정적 빌드, `CGO_ENABLED=0`)을 claude와 함께 컨테이너에 복사해 두고, 장수 exec으로 내부 watcher 서브커맨드를 돌린다. watcher는 config dir을 inotify로 감시해 변경 이벤트를 stdout에 찍고, 데몬이 그 스트림을 읽어 **debounce(기본 3초) 후 복사-아웃**한다. 대화 턴이 끝날 때마다 몇 초 안에 백업이 따라온다.
  - pane 프로세스(cld exec 클라이언트)가 끝나면(claude 종료·컨테이너 정지) 데몬에 통지 → 즉시 최종 복사-아웃 + `ls` 상태 `session-ended` 반영.
  - `die` 이벤트에서도 최종 복사 시도(멈춘 컨테이너에도 `docker cp`는 동작). `docker rm -f`(VS Code의 rebuild가 이 경로)는 die→destroy가 순식간이라 이 복사는 실패할 수 있지만, watcher 덕에 손실은 마지막 debounce 몇 초분으로 줄어든다.
  - 컨테이너 arch가 호스트와 달라 cld 자신을 못 돌리는 경우만 fallback으로 주기 스냅샷(60초).
  - jsonl은 append-only라 실행 중 스냅샷도 안전.
- **복사-인**: `ensure()`에서 첫 세션 생성 전에 복원.
- **백업 저장소는 global과 per-project로 분리** (`$XDG_DATA_HOME` = `~/.local/share/cld/` — 지워지면 안 되는 데이터라 cache가 아님):
  - `global/`: `.credentials.json`, `.claude.json`(전역 키), `settings.json`, `CLAUDE.md`·`agents/`·`skills/` 등 프로젝트 무관 상태. 어느 컨테이너에서 나왔든 마지막 것이 최신.
  - `projects/<local_folder 해시>/`: `projects/<인코딩된 cwd>/` 트랜스크립트(+세션 하위 디렉터리), `file-history/`(`/rewind`용).
  - 이렇게 나눠야 **새 프로젝트의 첫 컨테이너도 로그인 없이 시작**한다(global만 복원) — 최초 1회 로그인이 이후 모든 프로젝트·컨테이너로 전파된다. 마운트 제거의 전제 조건.
  - 복원 = global 복사 → 해당 프로젝트 오버레이 → `.claude.json`은 global 백업본 위에 이 프로젝트의 시드 키 병합.
  - 제외: `shell-snapshots/`, `sessions/`, `session-env/`, 캐시류, legacy `todos/`·`statsig/`.
- 새 컨테이너의 워크스페이스 경로가 백업 당시와 다르면 인코딩 디렉터리 rename + jsonl 안의 `cwd` 문자열 치환. (같은 devcontainer 구성이면 경로가 같으므로 드문 케이스 — 단순 구현으로 충분.)
- 한계: 같은 local_folder로 컨테이너를 동시에 2개 띄우면 백업은 마지막에 복사-아웃된 쪽이 이긴다.

## ls 명령어

`~/.cache/cld/cld.sock`으로 데몬에 조회. 출력:

- 이름 (= `basename(devcontainer.local_folder)`, 충돌 시 `-<짧은 ID>` 접미사)
- container ID
- 상태: `provisioning` / `ready` / `session-ended`(사용자가 종료) / `failed`(사유 로그)
- 프로비저닝된 claude 버전

compose에서 워크스페이스 컨테이너만 나오는 건 라벨 필터로 자동 충족.

## it 명령어

`cld it <이름>` = `tmux -S ~/.cache/cld/tmux.sock attach -t cld-<이름>` 을 `syscall.Exec`으로 실행. 데몬 불필요, docker 권한도 불필요.

- 세션이 없으면 상태에 따라 힌트 출력: 컨테이너가 안 돌고 있으면 그 사실을, 사용자가 종료한 세션이면 재생성 방법을 안내.
- (옵션) `cld it --new <이름>`: 데몬에 세션 재생성 요청 후 attach.

## 사후 마운트에 대해 (조사 결과)

"컨테이너가 만들어진 뒤에 호스트 디렉터리를 컨테이너 FS에 마운트"는 Docker API/CLI로는 불가능하다 — 마운트는 생성 시점에 고정. 우회 기법:

1. **devcontainer.json / compose에 선언** (생성 시점 마운트) — devcontainer 네이티브 방식. (지금까지 `~/.claude` 공유를 이걸로 해왔지만, cld의 동기화가 대체할 예정.)
2. **`docker cp`** — 마운트가 아니라 복사(라이브 동기화 없음). 바이너리 주입엔 이걸로 충분. ← **v1 채택**
3. **mount-namespace 수술** — 호스트에서 `open_tree(OPEN_TREE_CLONE)`로 소스 fd를 딴 뒤(소스가 호스트 ns에서 resolve됨) 컨테이너 mount ns로 `setns` + `move_mount(MOVE_MOUNT_F_EMPTY_PATH)`. 커널 5.2+, root(CAP_SYS_ADMIN) 필요. Go 주의점: 멀티스레드 Go 프로세스는 스레드끼리 fs_struct(CLONE_FS)를 공유해서 `setns(mntns)`가 EINVAL로 실패한다 — `runtime.LockOSThread`로도 안 되고, fork/exec한 단일 목적 헬퍼 프로세스에서 수행해야 한다. 진짜 라이브(양방향, 실시간) 마운트가 필요해지면 이 방법이 유일하나 v1 범위 밖.
   - 참고로 안 되는 것들: `nsenter -m -t <pid> mount --bind`(소스가 컨테이너 ns에서 resolve돼 실패), util-linux `mount -N`(같은 이유로 현재 버그, util-linux#3884), overlay2 MergedDir에 호스트에서 마운트(private propagation이라 컨테이너에 안 보임), mknod 블록 디바이스 트릭(tmpfs/overlay 소스 불가, 구식).

## 테스트

- 개발 devcontainer에 **DinD 사이드카**(`docker:28-dind`, TLS 없이 `tcp://docker:2375`)를 붙이고 `DOCKER_HOST`로 가리킨다 — `.devcontainer/docker-compose.yaml`에 반영됨. cld는 SDK로 API만 쓰므로 로컬 소켓과 원격 엔진의 차이가 없다.
- 통합 테스트: 사이드카 엔진에 `devcontainer.local_folder` 라벨만 단 가짜 devcontainer 컨테이너를 띄워 감지 → 복사 → 세션 생성 → 정리 전 과정을 검증한다. 실제 claude 다운로드는 더미 바이너리로 대체할 수 있게 다운로드 base URL을 config로 뺀다.
- 순수 로직(이름 도출, 라벨/마운트 파싱, 버전 비교, `.claude.json` 병합)은 일반 unit test.

## 하지 않는 것

- 보안 경화 (소켓/캐시 권한, peer 검증) — 범위 밖. 단일 사용자 호스트 가정.
