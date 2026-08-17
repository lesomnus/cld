# 세션 전용 Docker 엔진 — 구현 계획

claude 세션이 도커를 쓸 수 있게, **프로젝트마다 격리된 docker-in-docker 엔진**을
cld가 띄우고 전용 네트워크로 연결해 준다. `devcontainer.json`을 고치지 않는다.
진행 상황은 [단계](#단계)의 체크박스로 갱신한다.

## 위험 고지 (문서에 반드시 남길 것)

**이 기능을 켜면 그 위험은 사용자가 감수하는 것이다.**

- 세션에 도커 엔진을 준다는 것은 **그 엔진의 root를 주는 것**이다. claude와 claude가
  실행하는 모든 코드가 컨테이너를 만들고, 워크스페이스를 마운트하고, 특권 컨테이너를
  띄울 수 있다.
- dind 컨테이너는 **`--privileged`로 뜨고 호스트 커널을 공유한다.** 별도 VM이 아니다.
  탈출 표면이 존재하며, cld는 그 위에 추가 샌드박스를 두지 않는다.
- 엔진은 **TLS 없이** 전용 네트워크의 2375에서 듣는다. 그 네트워크에 붙은 것(=해당
  devcontainer)은 무엇이든 엔진을 완전히 조종할 수 있다.
- 워크스페이스가 엔진 컨테이너에도 bind되므로, 엔진을 조종할 수 있으면 워크스페이스
  파일에 접근할 수 있다.

호스트 소켓을 마운트하는 것(= 호스트 엔진 전체 장악)보다는 낫지만, "안전하다"가
아니라 "덜 위험하다"이다. 기본값이 `off`인 이유이고, 켜는 것은 사용자의 판단이다.

## 왜 이 구조인가

cld는 컨테이너를 만들지 않으므로 이미 떠 있는 devcontainer에 볼륨을 붙일 수 없다.
반면 **네트워크는 실행 중인 컨테이너에도 붙는다**(`NetworkConnect`). 그래서:

```
[devcontainer] --(전용 브리지 네트워크)-- [<key>-dind]
      claude가 DOCKER_HOST=tcp://<key>-dind:2375 로 사용
```

지금 이 저장소의 devcontainer가 compose로 하고 있는 것(dind 사이드카 +
`DOCKER_HOST=tcp://docker:2375`)과 같은 구조를, compose 파일 없이 임의의
프로젝트에 붙여 주는 것이다.

소켓 릴레이(`cld x docker`로 exec stdio 위에 엔진 API를 중개)는 **범위 밖**이다.

## 확정된 결정

- **프로젝트당 엔진 하나.** 프로젝트 격리가 cld의 다른 상태(백업, 프록시 설정)와
  같은 단위다. 이미지 캐시는 프로젝트별 named volume으로 세션 간 보존한다.
- **`--privileged` 기본.** rootless dind는 스토리지 드라이버·네트워크 제약이 있어
  기본으로 두면 "왜 안 되지"가 잦다. 이미지는 설정 가능하므로 rootless를 쓰고 싶으면
  `image: docker:dind-rootless`로 바꾼다.
- **`mode: off` 기본.** 옵트인이다.
- **`DOCKER_HOST`는 사용자 설정보다 낮은 레이어에 넣는다.** 원격 엔진을 직접
  가리키려는 사용자(`env: {DOCKER_HOST: tcp://build01…}`)가 이긴다. dind는 기본값을
  제공할 뿐이다. 따라서 `DOCKER_HOST`는 예약 키가 아니다.
- **호스트 이름은 컨테이너 이름 전체를 쓴다.** `docker`라는 짧은 별칭은 devcontainer가
  이미 붙어 있는 다른 네트워크의 이름과 충돌해 DNS가 모호해질 수 있다.
- 컨테이너 안의 `docker` CLI는 devcontainer feature의 몫이다. cld는 엔진만 가리킨다.

## 형식

```yaml
docker:
  mode: dind          # off(기본) | dind
  image: docker:28-dind

projects:
  - match: ~/work/infra/**
    docker:
      mode: dind      # 이 프로젝트에만
```

전역 → 매칭된 프로젝트 순으로 필드 단위 덮어쓰기(빈 값 = 미지정).

## 자원과 이름

키는 `backup_key(e)`(프로젝트 단위 안정 키)를 쓴다.

| 자원 | 이름 | `cld down` | `cld purge` |
| --- | --- | --- | --- |
| 엔진 컨테이너 | `<key>-dind` | 제거 | 제거 |
| 네트워크 | `<key>-dind-net` | 제거 | 제거 |
| 이미지 캐시 볼륨 | `<key>-dind-data` | **유지** | 제거 |

라벨 `cld.dind=<key>`로 찾는다. `devcontainer.local_folder` 라벨이 없으므로 데몬이
이 컨테이너를 devcontainer로 오인하지 않는다.

## 프로비저닝 단계 (`ensure_dind`)

`ensure_`에서 세션 생성 **전에**, `install_files`/`scripts` 다음에 실행한다.

1. 네트워크가 없으면 만든다(브리지, 라벨).
2. 엔진 컨테이너가 없으면 만든다: `--privileged`, `DOCKER_TLS_CERTDIR=""`(→ 2375
   평문), 볼륨 `<key>-dind-data` → `/var/lib/docker`, 네트워크 연결, 그리고
   **워크스페이스 bind**(아래).
3. 멈춰 있으면 시작한다.
4. devcontainer를 그 네트워크에 연결한다(이미 연결돼 있으면 무시).
5. 준비될 때까지 짧게 기다린다: 엔진 컨테이너 안에서 `docker info`를 재시도.
   best-effort — 실패해도 프로비저닝을 막지 않는다(로그만).

### 워크스페이스 bind가 핵심이다

dind 안에서 `docker run -v $(pwd):/app`을 하면 그 경로는 **엔진 컨테이너의
파일시스템**에서 resolve된다. devcontainer의 `/workspace`가 아니다. 그대로 두면
빈 디렉터리가 마운트되고 testcontainers·compose 워크플로가 조용히 깨진다.

그래서 엔진 컨테이너에 호스트 워크스페이스(`item.LocalFolder`)를 **devcontainer와
같은 컨테이너 경로**(`item.Workspace`)로 bind한다. cld는 둘 다 알고 있다.
`cld up`이 러너 컨테이너에 쓰는 것과 같은 발상이다(`plan.md`).

## 단계

- [x] 1. 계획 문서 (이 파일)
- [ ] 2. 설정 스키마 — `docker:` 전역/프로젝트, 해석과 검증
- [ ] 3. `ensure_dind` — 네트워크·볼륨·엔진 생성, 연결, 준비 대기, `DOCKER_HOST` 레이어
- [ ] 4. 정리 — `down`/`purge`가 엔진·네트워크·볼륨을 함께 처리
- [ ] 5. 통합 테스트 — 실제 엔진에 대고 생성·연결·재사용·정리
- [ ] 6. 사용자 문서 — `docs/session-docker.md`, README, cld.yaml, 위험 고지

## 범위 밖

- 소켓 릴레이 전송(`cld x docker`).
- 엔진 API 필터링/감사(특권 컨테이너 금지 등). 데이터 경로에 cld가 없는 구조라
  애초에 불가능하다.
- 컨테이너에 docker CLI 주입 — devcontainer feature의 몫.
- 전 프로젝트 공유 엔진.
