package main

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ectypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// ===== 여기 환경 맞게 수정 =====
const (
	region      = "ap-northeast-2"
	clusterName = "test" // 이미 존재하는 ECS 클러스터 이름

	taskFamily    = "curl-routing-test" // 새로 만들 태스크 패밀리 이름
	containerName = "curl"

	executionRoleArn = "arn:aws:iam::<account-id>:role/<execution-role-name>"
	taskRoleArn      = "arn:aws:iam::<account-id>:role/<task-role-name>"

	subnet1        = "<subnet-id>"
	securityGroup1 = "<security-group-id>"

	// CloudWatch Logs
	logGroupName = "/ecs/curl-routing-test"

	// 실제로 라우팅 테스트할 URL
	testURL = "https://<host>"
)

func main() {
	ctx := context.Background()

	// 1. AWS 설정 로드
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	ecsClient := ecs.NewFromConfig(cfg)

	// 2. Fargate TaskDefinition 등록 (curl 전용)
	taskDefArn, err := registerCurlTaskDef(ctx, ecsClient)
	if err != nil {
		log.Fatalf("failed to register task definition: %v", err)
	}
	log.Printf("registered task definition: %s\n", aws.ToString(taskDefArn))

	// 3. Fargate 태스크 한 개 실행 (awsvpc 네트워크 모드)
	if err := runCurlTask(ctx, ecsClient, aws.ToString(taskDefArn)); err != nil {
		log.Fatalf("failed to run task: %v", err)
	}

	log.Println("RunTask 호출 완료. ECS 콘솔 / CloudWatch Logs에서 curl 결과를 확인하세요.")
}

// curl을 실행하는 Fargate TaskDefinition 생성
func registerCurlTaskDef(ctx context.Context, ecsClient *ecs.Client) (*string, error) {
	// Fargate에서 쓸 수 있는 CPU/Memory 조합 예시 (0.25 vCPU / 0.5GB)
	cpu := "256"
	memory := "512"

	// 컨테이너가 실행할 커맨드:
	// - curl -v <testURL>
	// - 600초 sleep 해서 로그 확인 시간 확보
	command := []string{
		"sh", "-c",
		fmt.Sprintf("echo '=== curl test start ===' && curl -v %s && echo '=== curl done ===' && sleep 600", testURL),
	}

	logConfig := &ectypes.LogConfiguration{
		LogDriver: ectypes.LogDriverAwslogs,
		Options: map[string]string{
			"awslogs-group":         logGroupName,
			"awslogs-region":        region,
			"awslogs-stream-prefix": "curl-test",
		},
	}

	input := &ecs.RegisterTaskDefinitionInput{
		Family:      aws.String(taskFamily),
		Cpu:         aws.String(cpu),
		Memory:      aws.String(memory),
		NetworkMode: ectypes.NetworkModeAwsvpc,
		RequiresCompatibilities: []ectypes.Compatibility{
			ectypes.CompatibilityFargate,
		},
		ExecutionRoleArn: aws.String(executionRoleArn),
		TaskRoleArn:      aws.String(taskRoleArn),

		RuntimePlatform: &ectypes.RuntimePlatform{
			CpuArchitecture:       ectypes.CPUArchitectureX8664, // curl 이미지는 멀티아치라 이걸로 충분
			OperatingSystemFamily: ectypes.OSFamilyLinux,
		},

		ContainerDefinitions: []ectypes.ContainerDefinition{
			{
				Name:             aws.String(containerName),
				Image:            aws.String("curlimages/curl:8.8.0"), // 경량 curl 이미지
				Essential:        aws.Bool(true),
				Command:          command,
				LogConfiguration: logConfig,
			},
		},
	}

	out, err := ecsClient.RegisterTaskDefinition(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("RegisterTaskDefinition: %w", err)
	}

	return out.TaskDefinition.TaskDefinitionArn, nil
}

// 위 TaskDefinition으로 Fargate Task 실행
func runCurlTask(ctx context.Context, ecsClient *ecs.Client, taskDefArn string) error {
	input := &ecs.RunTaskInput{
		Cluster:         aws.String(clusterName),
		TaskDefinition:  aws.String(taskDefArn),
		LaunchType:      ectypes.LaunchTypeFargate,
		Count:           aws.Int32(1),
		PlatformVersion: aws.String("LATEST"),

		NetworkConfiguration: &ectypes.NetworkConfiguration{
			AwsvpcConfiguration: &ectypes.AwsVpcConfiguration{
				Subnets: []string{
					subnet1,
				},
				SecurityGroups: []string{
					securityGroup1,
				},
				AssignPublicIp: ectypes.AssignPublicIpDisabled, // NAT 경유 프라이빗 서브넷 테스트
			},
		},
	}

	out, err := ecsClient.RunTask(ctx, input)
	if err != nil {
		return fmt.Errorf("RunTask: %w", err)
	}

	if len(out.Failures) > 0 {
		return fmt.Errorf("RunTask failures: %#v", out.Failures)
	}

	for _, t := range out.Tasks {
		log.Printf("started task: %s (lastStatus=%s)", aws.ToString(t.TaskArn), aws.ToString(t.LastStatus))
	}

	return nil
}
