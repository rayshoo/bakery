# Bakery

분산 컨테이너 이미지 빌드 시스템입니다. Kaniko 기반으로 Docker-in-Docker 없이 컨테이너 이미지를 빌드하며, AWS ECS 또는 Kubernetes 위에서 멀티 아키텍처(amd64, arm64) 빌드를 병렬로 실행합니다.

## 프로젝트 배경

CI/CD 환경에서 컨테이너 이미지를 빌드할 때 보통 Docker-in-Docker(DinD)를 사용하거나 빌드 머신에 Docker 데몬을 직접 띄워야 합니다. 이 방식은 권한 문제, 보안 이슈, 리소스 낭비 등의 문제가 있습니다.

이 프로젝트는 다음 문제를 해결하기 위해 시작되었습니다:

- **DinD 제거**: Kaniko를 사용해 Docker 데몬 없이 이미지를 빌드합니다. privileged 모드가 필요 없습니다.
- **멀티 아키텍처 빌드**: amd64와 arm64 이미지를 각각의 네이티브 환경에서 병렬로 빌드합니다. QEMU 에뮬레이션 없이 빠르게 빌드할 수 있습니다.
- **빌드 인프라 분리**: 빌드 작업을 ECS나 Kubernetes에 위임하여 CI 러너의 리소스를 절약하고, 빌드 환경을 독립적으로 스케일링할 수 있습니다.
- **docker-compose.yaml 호환**: 기존 docker-compose.yaml을 그대로 활용하여 멀티 서비스 빌드를 설정할 수 있습니다.

## 아키텍처

```
┌──────────┐         ┌──────────────┐         ┌─────────────────┐
│  Client  │───S3───>│    Server    │──ECS──> │  Agent (amd64)  │
│          │──HTTP──>│ (Controller) │  or K8S │  Agent (arm64)  │
│          │<──Logs──│              │<──Logs──│  (Kaniko 실행)   │
└──────────┘         └──────────────┘         └─────────────────┘
```

**Client**: 소스코드를 tar.gz로 압축해 S3에 업로드하고, Server에 빌드를 요청합니다. 빌드 로그를 실시간 스트리밍으로 수신하여 출력합니다.

**Server (Controller)**: 빌드 요청을 받아 아키텍처별 태스크를 생성하고, ECS 또는 Kubernetes에 Agent 컨테이너를 실행합니다. Agent의 로그를 수집하여 Client에 전달합니다.

**Agent**: S3에서 소스코드를 다운로드하고 Kaniko로 이미지를 빌드합니다. pre/post 스크립트 실행을 지원하며, 빌드 결과를 Server에 보고합니다.

## 설정

### 환경 변수

`.env.example`을 `.env`로 복사하여 환경에 맞게 수정합니다.

```bash
cp .env.example .env
```

**공통 (Server, Client 모두 필요)**

| 변수 | 설명 |
|---|---|
| `S3_ENDPOINT` | S3 엔드포인트 (예: `s3.amazonaws.com`) |
| `S3_REGION` | S3 리전 |
| `S3_BUCKET` | 빌드 컨텍스트를 저장할 S3 버킷 |
| `S3_SSL` | SSL 사용 여부 (`true`/`false`) |
| `CONTROLLER_URL` | Server의 공개 URL |

**Server 전용**

| 변수 | 설명 |
|---|---|
| `AWS_REGION` | AWS 리전 |
| `ECS_CLUSTER` | ECS 클러스터 이름 |
| `ECS_SUBNETS` | ECS 서브넷 (쉼표 구분) |
| `ECS_SECURITY_GROUPS` | ECS 보안 그룹 (쉼표 구분) |
| `ECS_EXEC_ROLE_ARN` | ECS 실행 역할 ARN |
| `ECS_TASK_ROLE_ARN` | ECS 태스크 역할 ARN |
| `AGENT_IMAGE` | Agent 컨테이너 이미지 |
| `AGENT_IMAGE_SECRET_ARN` | Agent 이미지 pull용 시크릿 ARN |
| `K8S_NAMESPACE` | Kubernetes 네임스페이스 |
| `BUILD_TASK_TIMEOUT` | 빌드 태스크 타임아웃 (기본: `10m`) |
| `BUILD_RESULT_TIMEOUT` | 빌드 결과 대기 타임아웃 (기본: `10m`) |
| `DEFAULT_BUILD_CPU` | 기본 CPU (기본: `0.5`) |
| `DEFAULT_BUILD_MEMORY` | 기본 메모리 (기본: `2G`) |

**Client 전용**

| 변수 | 설명 |
|---|---|
| `LOG_FORMAT` | 로그 형식 (`simple`, `plain`, `json`) |

### 빌드 설정 파일 (config.yaml)

`client-config.yaml.example`을 참고하여 `config.yaml`을 작성합니다.

```yaml
global:
  # 실행 플랫폼: ecs 또는 k8s
  platform: ecs

  # 기본 아키텍처
  arch: amd64

  # Agent 컨테이너에 전달할 환경 변수
  env:
    FOO: bar

  # Kaniko 실행 전 스크립트
  pre-script: |
    echo 'setting up...'

  # Kaniko 빌드 성공 후 스크립트
  post-script: |
    echo 'done'

  # 프라이빗 레지스트리 인증 정보
  kaniko-credentials:
  - registry: registry.example.com
    username: user
    password: pass

  # Kaniko 빌드 옵션
  kaniko:
    context-path: /workspace/src
    dockerfile: Dockerfile
    destination: registry.example.com/myapp:latest
    build-args:
      BASE_IMAGE: alpine:latest
    cache:
      enable: true
      repo: cache.example.com
      ttl: 24h

# 아키텍처별 빌드 설정 (global 설정을 상속하며, 동일 키는 override)
bake:
- arch: amd64
  kaniko:
    build-args:
      BUILD_PLATFORM: amd64

- arch: arm64
  kaniko:
    dockerfile: Dockerfile.arm64
    build-args:
      BUILD_PLATFORM: arm64
```

`bake` 항목의 각 설정은 `global` 설정을 상속받으며, 동일한 키가 있으면 override됩니다. `env`, `build-args` 같은 맵 타입은 병합(merge)되고, 나머지는 덮어씁니다.

### docker-compose.yaml 모드

기존 docker-compose.yaml을 그대로 사용하여 빌드할 수 있습니다. `x-bake.platforms`로 아키텍처를 지정합니다.

```yaml
services:
  myapp:
    build:
      context: .
      dockerfile: Dockerfile
      args:
        VERSION: "1.0.0"
      x-bake:
        platforms:
        - linux/amd64
        - linux/arm64
    image: registry.example.com/myapp:1.0.0
```

## AWS ECS 설정

ECS를 빌드 플랫폼으로 사용할 경우 아래의 AWS 리소스와 IAM 권한이 필요합니다. `example/terraform/`에 참고용 Terraform 구성이 포함되어 있습니다.

### 인프라

- **ECS 클러스터**: Fargate 및 Fargate Spot 용량 공급자
- **VPC 서브넷**: Agent 컨테이너가 S3, Controller, 컨테이너 레지스트리에 접근할 수 있도록 인터넷 액세스(또는 NAT 게이트웨이)가 가능한 서브넷
- **S3 버킷**: 빌드 컨텍스트 저장용

### IAM 역할

Server용 권한 1개와 Agent용 ECS 태스크 역할 2개, 총 3개의 권한 세트가 필요합니다.

#### 1. Server (Controller)

Server가 실행되는 환경(EC2 인스턴스 프로파일, ECS 태스크 역할, CI 러너 역할 등)에 아래 권한이 필요합니다.

**ECS** — 태스크 정의 관리 및 Agent 태스크 실행:

| Action | 용도 |
|---|---|
| `ecs:RegisterTaskDefinition` | 아키텍처/리소스 조합별 태스크 정의 생성 |
| `ecs:DescribeTaskDefinition` | 태스크 정의 존재 여부 확인 |
| `ecs:DeregisterTaskDefinition` | 서버 시작 시 기존 태스크 정의 정리 |
| `ecs:ListTaskDefinitions` | 정리 대상 태스크 정의 조회 |
| `ecs:ListTaskDefinitionFamilies` | 정리 대상 태스크 정의 패밀리 조회 |
| `ecs:RunTask` | Fargate에서 Agent 컨테이너 실행 |
| `ecs:DescribeTasks` | Agent 태스크 상태 모니터링 |

**Secrets Manager** — Agent 이미지 pull을 위한 프라이빗 레지스트리 인증 관리:

| Action | 용도 |
|---|---|
| `secretsmanager:CreateSecret` | `AGENT_IMAGE_SECRET_ARN`이 미지정 시 새 시크릿 생성 |
| `secretsmanager:DescribeSecret` | 기존 시크릿의 ARN 조회 |

**IAM**:

| Action | 용도 |
|---|---|
| `iam:PassRole` | 태스크 정의 등록 시 실행 역할과 태스크 역할을 ECS에 전달 |

**CloudWatch Logs** (선택, `ECS_LOG_GROUP` 설정 시):

| Action | 용도 |
|---|---|
| `logs:CreateLogStream` | Agent 컨테이너의 로그 스트림 생성 |
| `logs:PutLogEvents` | Agent 로그를 CloudWatch에 기록 |

#### 2. Agent 실행 역할 (`ECS_EXEC_ROLE_ARN`)

ECS가 Agent 컨테이너 이미지를 pull하고 로그를 전송할 때 사용하는 역할입니다. 태스크 정의의 `executionRoleArn`에 지정됩니다.

| 권한 | 용도 |
|---|---|
| `AmazonECSTaskExecutionRolePolicy` (관리형 정책) | ECR에서 컨테이너 이미지 pull, CloudWatch 로그 기록 |
| `secretsmanager:GetSecretValue` (`AGENT_IMAGE_SECRET_ARN` 대상) | 프라이빗 레지스트리에서 Agent 이미지 pull (선택) |

#### 3. Agent 태스크 역할 (`ECS_TASK_ROLE_ARN`)

Agent 컨테이너가 런타임에 S3에서 빌드 컨텍스트를 다운로드하기 위해 사용하는 역할입니다.

| Action | Resource | 용도 |
|---|---|---|
| `s3:GetObject` | `arn:aws:s3:::<bucket>/*` | 빌드 컨텍스트 다운로드 |
| `s3:ListBucket` | `arn:aws:s3:::<bucket>` | 빌드 컨텍스트 버킷 내 객체 목록 조회 |

### Client 권한

Client는 빌드 컨텍스트를 S3에 업로드하기 위한 권한이 필요합니다.

| Action | Resource | 용도 |
|---|---|---|
| `s3:PutObject` | `arn:aws:s3:::<bucket>/*` | 빌드 컨텍스트 tar.gz 업로드 |

### 보안 그룹

Agent 보안 그룹은 아웃바운드 인터넷 액세스만 필요합니다. 인바운드 규칙은 필요하지 않습니다.

| 방향 | 프로토콜 | 포트 | 대상 | 용도 |
|---|---|---|---|---|
| Egress | All | All | `0.0.0.0/0` | S3, Controller, 컨테이너 레지스트리 접근 |

## Kubernetes 배포

`example/k8s/`에 Kustomize 기반의 Controller Server 배포 예시가 포함되어 있습니다.

### 디렉토리 구조

```
example/k8s/
├── .env                  # Server 환경 변수 (Secret으로 생성)
├── configs/
│   └── config.yaml       # K8s Agent 설정 (ConfigMap으로 생성)
├── deploy.yaml           # Deployment
├── sa.yaml               # ServiceAccount (Server, Agent)
├── role.yaml             # Role (Server, Agent)
├── rolebinding.yaml      # RoleBinding
├── svc.yaml              # Service (headless)
├── ing.yaml              # Ingress
└── kustomization.yaml
```

### 환경 변수 설정

`.env` 파일에 Server에 필요한 환경 변수를 작성합니다. Kustomize의 `secretGenerator`가 이 파일을 읽어 Kubernetes Secret으로 생성합니다.

```
S3_ENDPOINT=s3.amazonaws.com
S3_REGION=ap-northeast-2
S3_BUCKET=my-build-bucket
S3_SSL=true
CONTROLLER_URL=https://build.example.com
AWS_REGION=ap-northeast-2
ECS_CLUSTER=build-cluster
ECS_SUBNETS=subnet-xxx,subnet-yyy
ECS_SECURITY_GROUPS=sg-xxx
ECS_EXEC_ROLE_ARN=arn:aws:iam::<account-id>:role/build-agent-execution
ECS_TASK_ROLE_ARN=arn:aws:iam::<account-id>:role/build-agent-task
AGENT_IMAGE=docker.io/rayshoo/bakery/agent:v1.0.0
CLEANUP_ECS_TASK_DEFINITIONS=true
```

`kustomization.yaml`에서 이 파일을 `secretGenerator`로 참조합니다:

```yaml
secretGenerator:
- name: build
  envs:
  - .env
```

### 배포

```bash
kubectl apply -k example/k8s/
```

## 사용법

### 로컬 실행

```bash
# Server 실행
make server

# 단일 config.yaml로 빌드 요청
make client

# docker-compose.yaml로 빌드 요청 (비동기)
make compose
```

### Client CLI 옵션

```bash
go run cmd/client/main.go \
  --config config.yaml \        # 빌드 설정 파일 (선택)
  --compose compose.yaml \      # docker-compose 파일 (선택)
  --services "app,worker" \     # 빌드할 서비스 필터 (선택, 비워두면 전체)
  --async \                     # 비동기 빌드 모드
  --repo .                      # 소스코드 경로 (기본: 현재 디렉토리)
```

`--config`와 `--compose`를 함께 사용하면, config.yaml의 global 설정이 base로 적용되고 compose 파일의 서비스별 설정이 merge됩니다.

### 컨테이너 이미지 빌드

```bash
# 전체 서비스 이미지 빌드 (server, client, agent) 후 레지스트리에 push
make bake

# Agent 이미지만 빌드
make agent
```

## 빌드 흐름

1. Client가 소스코드를 tar.gz로 압축하여 S3에 업로드합니다
2. Client가 빌드 설정(YAML)과 함께 Server에 POST 요청을 보냅니다
3. Server가 아키텍처별 빌드 태스크를 생성합니다
4. Server가 ECS 또는 Kubernetes에 Agent 컨테이너를 실행합니다
5. Agent가 S3에서 소스코드를 다운로드하고 Kaniko로 빌드합니다
6. Agent가 빌드 로그를 실시간으로 Server에 전송합니다
7. Client가 Server에서 로그를 스트리밍으로 수신합니다
8. 빌드 완료 후 이미지가 지정된 레지스트리에 push됩니다
