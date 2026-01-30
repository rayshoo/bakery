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

// ===== Adjust these values for your environment =====
const (
	region      = "ap-northeast-2"
	clusterName = "test" // Name of an existing ECS cluster

	taskFamily    = "curl-routing-test" // Task family name to create
	containerName = "curl"

	executionRoleArn = "arn:aws:iam::<account-id>:role/<execution-role-name>"
	taskRoleArn      = "arn:aws:iam::<account-id>:role/<task-role-name>"

	subnet1        = "<subnet-id>"
	securityGroup1 = "<security-group-id>"

	// CloudWatch Logs
	logGroupName = "/ecs/curl-routing-test"

	// URL to test routing against
	testURL = "https://<host>"
)

func main() {
	ctx := context.Background()

	// 1. Load AWS config
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	ecsClient := ecs.NewFromConfig(cfg)

	// 2. Register Fargate TaskDefinition (curl-only)
	taskDefArn, err := registerCurlTaskDef(ctx, ecsClient)
	if err != nil {
		log.Fatalf("failed to register task definition: %v", err)
	}
	log.Printf("registered task definition: %s\n", aws.ToString(taskDefArn))

	// 3. Run a single Fargate task (awsvpc network mode)
	if err := runCurlTask(ctx, ecsClient, aws.ToString(taskDefArn)); err != nil {
		log.Fatalf("failed to run task: %v", err)
	}

	log.Println("RunTask invoked. Check the ECS console / CloudWatch Logs for curl results.")
}

// registerCurlTaskDef creates a Fargate TaskDefinition that runs curl.
func registerCurlTaskDef(ctx context.Context, ecsClient *ecs.Client) (*string, error) {
	// Valid Fargate CPU/Memory combination (0.25 vCPU / 0.5GB)
	cpu := "256"
	memory := "512"

	// Container command:
	// - curl -v <testURL>
	// - sleep 600s to allow time for log inspection
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
			CpuArchitecture:       ectypes.CPUArchitectureX8664, // curl image is multi-arch, x86_64 is sufficient
			OperatingSystemFamily: ectypes.OSFamilyLinux,
		},

		ContainerDefinitions: []ectypes.ContainerDefinition{
			{
				Name:             aws.String(containerName),
				Image:            aws.String("curlimages/curl:8.8.0"), // Lightweight curl image
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

// runCurlTask runs a Fargate task using the given TaskDefinition.
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
				AssignPublicIp: ectypes.AssignPublicIpDisabled, // Test via NAT in private subnet
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
