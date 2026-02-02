# Bakery

A distributed container image build system. Builds container images using Kaniko without Docker-in-Docker, and runs multi-architecture (amd64, arm64) builds in parallel on AWS ECS or Kubernetes.

## Background

Building container images in CI/CD typically requires Docker-in-Docker (DinD) or a Docker daemon on the build machine. This introduces privileged mode requirements, security concerns, and resource waste.

This project addresses these problems:

- **No DinD**: Uses Kaniko to build images without a Docker daemon. No privileged mode needed.
- **Multi-architecture builds**: Builds amd64 and arm64 images natively in parallel — no QEMU emulation.
- **Separated build infrastructure**: Offloads build tasks to ECS or Kubernetes, freeing CI runner resources and allowing independent scaling.
- **docker-compose.yaml compatible**: Use your existing docker-compose.yaml to configure multi-service builds.

## Architecture

```
┌──────────┐         ┌──────────────┐         ┌─────────────────┐
│  Client  │───S3───>│    Server    │──ECS──> │  Agent (amd64)  │
│          │──HTTP──>│ (Controller) │  or K8S │  Agent (arm64)  │
│          │<──Logs──│              │<──Logs──│  (Kaniko)       │
└──────────┘         └──────────────┘         └─────────────────┘
```

**Client**: Compresses source code into tar.gz, uploads it to S3, and submits a build request to the Server. Receives build logs via real-time streaming.

**Server (Controller)**: Receives build requests, creates per-architecture tasks, and launches Agent containers on ECS or Kubernetes. Collects logs from Agents and forwards them to the Client.

**Agent**: Downloads source code from S3 and builds images with Kaniko. Supports pre/post build scripts and reports results back to the Server.

## Configuration

### Environment Variables

Copy `.env.example` to `.env` and edit for your environment.

```bash
cp .env.example .env
```

**Common (both Server and Client)**

| Variable | Description |
|---|---|
| `S3_ENDPOINT` | S3 endpoint (e.g. `s3.amazonaws.com`) |
| `S3_REGION` | S3 region |
| `S3_BUCKET` | S3 bucket for build context storage |
| `S3_SSL` | Enable SSL (`true`/`false`) |
| `CONTROLLER_URL` | Public URL of the Server |

**Server only**

| Variable | Description |
|---|---|
| `AWS_REGION` | AWS region |
| `ECS_CLUSTER` | ECS cluster name |
| `ECS_SUBNETS` | ECS subnets (comma-separated) |
| `ECS_SECURITY_GROUPS` | ECS security groups (comma-separated) |
| `ECS_EXEC_ROLE_ARN` | ECS execution role ARN |
| `ECS_TASK_ROLE_ARN` | ECS task role ARN |
| `AGENT_IMAGE` | Agent container image |
| `AGENT_IMAGE_SECRET_ARN` | Secret ARN for Agent image pull |
| `K8S_NAMESPACE` | Kubernetes namespace |
| `BUILD_TASK_TIMEOUT` | Build task timeout (default: `10m`) |
| `BUILD_RESULT_TIMEOUT` | Build result wait timeout (default: `10m`) |
| `DEFAULT_BUILD_CPU` | Default CPU (default: `0.5`) |
| `DEFAULT_BUILD_MEMORY` | Default memory (default: `2G`) |

**Client only**

| Variable | Description |
|---|---|
| `LOG_FORMAT` | Log format (`simple`, `plain`, `json`) |

### Build Config File (config.yaml)

Refer to `client-config.yaml.example` to create your `config.yaml`.

```yaml
global:
  # Execution platform: ecs or k8s
  platform: ecs

  # Default architecture
  arch: amd64

  # Environment variables passed to the Agent container
  env:
    FOO: bar

  # Script to run before Kaniko execution
  pre-script: |
    echo 'setting up...'

  # Script to run after successful Kaniko build
  post-script: |
    echo 'done'

  # Private registry credentials
  kaniko-credentials:
  - registry: registry.example.com
    username: user
    password: pass

  # Kaniko build options
  kaniko:
    context-path: .
    dockerfile: Dockerfile
    destination: registry.example.com/myapp:latest
    build-args:
      BASE_IMAGE: alpine:latest
    cache:
      enable: true
      repo: cache.example.com
      ttl: 24h

# Per-architecture build config (inherits from global, same keys override)
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

Each entry in `bake` inherits from the `global` config. Map types like `env` and `build-args` are merged; other values are overwritten.

### docker-compose.yaml Mode

You can use an existing docker-compose.yaml for builds. Specify architectures with `x-bake.platforms`.

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

## AWS ECS Setup

When using ECS as the build platform, the following AWS resources and IAM permissions are required. A reference Terraform configuration is available in `examples/server/terraform/`.

### Infrastructure

- **ECS Cluster**: Fargate and Fargate Spot capacity providers
- **VPC Subnets**: Subnets with internet access (or NAT gateway) for Agent containers to reach S3, the Controller, and container registries
- **S3 Bucket**: For build context storage

### IAM Roles

Three separate sets of permissions are required — one for the Server, and two ECS task roles for the Agent.

#### 1. Server (Controller)

The Server itself needs the following permissions on the machine or service where it runs (e.g. EC2 instance profile, ECS task role, or CI runner role):

**ECS** — to manage task definitions and run Agent tasks:

| Action | Purpose |
|---|---|
| `ecs:RegisterTaskDefinition` | Create task definitions per architecture/resource combination |
| `ecs:DescribeTaskDefinition` | Check if a task definition already exists |
| `ecs:DeregisterTaskDefinition` | Cleanup old task definitions on startup |
| `ecs:ListTaskDefinitions` | List task definitions for cleanup |
| `ecs:ListTaskDefinitionFamilies` | List task definition families for cleanup |
| `ecs:RunTask` | Launch Agent containers on Fargate |
| `ecs:DescribeTasks` | Monitor Agent task status |

**Secrets Manager** — to manage private registry credentials for Agent image pull:

| Action | Purpose |
|---|---|
| `secretsmanager:CreateSecret` | Create a new secret if `AGENT_IMAGE_SECRET_ARN` is not provided |
| `secretsmanager:DescribeSecret` | Retrieve the ARN of an existing secret |

**IAM**:

| Action | Purpose |
|---|---|
| `iam:PassRole` | Pass the execution role and task role to ECS when registering task definitions |

**CloudWatch Logs** (optional, when `ECS_LOG_GROUP` is set):

| Action | Purpose |
|---|---|
| `logs:CreateLogStream` | Create log streams for Agent containers |
| `logs:PutLogEvents` | Write Agent logs to CloudWatch |

#### 2. Agent Execution Role (`ECS_EXEC_ROLE_ARN`)

This role is used by ECS itself to pull the Agent container image and send logs. It is specified as the `executionRoleArn` in the task definition.

| Permission | Purpose |
|---|---|
| `AmazonECSTaskExecutionRolePolicy` (managed) | Pull container images from ECR, write CloudWatch logs |
| `secretsmanager:GetSecretValue` on `AGENT_IMAGE_SECRET_ARN` | Pull Agent image from a private registry (optional) |

#### 3. Agent Task Role (`ECS_TASK_ROLE_ARN`)

This role is assumed by the Agent container at runtime to download build context from S3.

| Action | Resource | Purpose |
|---|---|---|
| `s3:GetObject` | `arn:aws:s3:::<bucket>/*` | Download build context |
| `s3:ListBucket` | `arn:aws:s3:::<bucket>` | List objects in the build context bucket |

### Client Permissions

The Client needs permission to upload build context to S3:

| Action | Resource | Purpose |
|---|---|---|
| `s3:PutObject` | `arn:aws:s3:::<bucket>/*` | Upload build context tar.gz |

### Security Group

The Agent security group requires outbound internet access only. No inbound rules are needed.

| Direction | Protocol | Port | Destination | Purpose |
|---|---|---|---|---|
| Egress | All | All | `0.0.0.0/0` | S3, Controller, container registries |

## Kubernetes Deployment

A Kustomize-based example for deploying the Controller Server is available in `examples/server/k8s/`.

### Directory Structure

```
examples/server/k8s/
├── .env                  # Server environment variables (created as Secret)
├── configs/
│   └── config.yaml       # K8s Agent config (created as ConfigMap)
├── deploy.yaml           # Deployment
├── sa.yaml               # ServiceAccount (Server, Agent)
├── role.yaml             # Role (Server, Agent)
├── rolebinding.yaml      # RoleBinding
├── svc.yaml              # Service (headless)
├── ing.yaml              # Ingress
└── kustomization.yaml
```

### Environment Variables

Write the Server environment variables in the `.env` file. Kustomize's `secretGenerator` reads this file and creates a Kubernetes Secret.

```
S3_ENDPOINT=s3.amazonaws.com
S3_REGION=ap-northeast-2
S3_BUCKET=my-build-bucket
S3_SSL=true
CONTROLLER_URL=https://bakery.example.com
AWS_REGION=ap-northeast-2
ECS_CLUSTER=bakery-cluster
ECS_SUBNETS=subnet-xxx,subnet-yyy
ECS_SECURITY_GROUPS=sg-xxx
ECS_EXEC_ROLE_ARN=arn:aws:iam::<account-id>:role/bakery-agent-execution
ECS_TASK_ROLE_ARN=arn:aws:iam::<account-id>:role/bakery-agent-task
AGENT_IMAGE=docker.io/rayshoo/bakery-agent:v1.0.2
CLEANUP_ECS_TASK_DEFINITIONS=true
```

In `kustomization.yaml`, this file is referenced via `secretGenerator`:

```yaml
secretGenerator:
- name: bakery
  envs:
  - .env
```

### Deploy

```bash
kubectl apply -k examples/server/k8s/
```

## Usage

### Examples

Deployment and CI/CD integration examples are available under `examples/`:

```
examples/
├── server/                  # Server deployment examples
│   ├── k8s/                 # Kubernetes (Kustomize) manifests
│   └── terraform/           # AWS ECS infrastructure (Terraform)
└── client/                  # Client CI/CD integration examples
    ├── .github-actions.yml  # GitHub Actions workflow
    └── .gitlab.yml          # GitLab CI/CD pipeline
```

- **Server**: See `examples/server/` for Kubernetes and Terraform-based deployment of the bakery-server.
- **Client**: See `examples/client/` for CI/CD pipeline examples that use bakery-client to build and push container images (GitHub Actions, GitLab CI).

### Client CLI Options

```bash
bakery-client \
  --config config.yaml \        # Build config file (optional)
  --compose compose.yaml \      # docker-compose file (optional)
  --services "app,worker" \     # Services to build (optional, empty = all)
  --async \                     # Async build mode
  --repo .                      # Source code path (default: current directory)
```

When `--config` and `--compose` are used together, the global settings from config.yaml serve as the base and compose service settings are merged on top.

## Build Flow

1. Client compresses source code into tar.gz and uploads to S3
2. Client sends a build request with config (YAML) to the Server
3. Server creates per-architecture build tasks
4. Server launches Agent containers on ECS or Kubernetes
5. Agent downloads source code from S3 and builds with Kaniko
6. Agent streams build logs to the Server in real-time
7. Client receives logs from the Server via streaming
8. On completion, the image is pushed to the specified registry

## Container Image Build

```bash
# Build all service images (bakery-server, bakery-client, bakery-agent) and push to registry
make bake
```