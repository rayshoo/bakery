package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"

	"github.com/rayshoo/bakery/internal/config"
	ecsExec "github.com/rayshoo/bakery/internal/ecs"
	k8s2 "github.com/rayshoo/bakery/internal/k8s"
	"github.com/rayshoo/bakery/internal/orchestrator"
	"github.com/rayshoo/bakery/internal/routes"
	"github.com/rayshoo/bakery/internal/state"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	smt "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/joho/godotenv"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var version = "dev"

// ServerReadiness 는 서버의 준비 상태를 관리하는 구조체입니다.
// Kubernetes readiness probe 에서 사용됩니다.
type ServerReadiness struct {
	ready bool
}

func (s *ServerReadiness) SetReady() {
	s.ready = true
}

func (s *ServerReadiness) IsReady() bool {
	return s.ready
}

var serverReadiness = &ServerReadiness{}

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	_ = godotenv.Load(".env")

	awsRegion := getenv("AWS_REGION", "ap-northeast-2")
	clusterName := getenv("ECS_CLUSTER", "build-cluster")

	log.Println("[main] starting build controller...")
	log.Println("[main] AWS_REGION =", awsRegion)
	log.Println("[main] ECS_CLUSTER =", clusterName)

	awsCfg, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion(awsRegion),
	)
	if err != nil {
		log.Fatalf("failed to load AWS config: %v", err)
	}

	if getenv("CLEANUP_ECS_TASK_DEFINITIONS", "false") == "true" {
		ecsClient := ecs.NewFromConfig(awsCfg)
		if err := cleanupECSTaskDefinitions(context.Background(), ecsClient); err != nil {
			log.Printf("[WARN] ECS task definition cleanup failed: %v", err)
		}
	}

	secrets := secretsmanager.NewFromConfig(awsCfg)

	secretArn, err := ensureRegistrySecret(context.Background(), secrets)
	if err != nil {
		log.Fatalf("secret ensure failed: %v", err)
	}

	ecsClient := ecs.NewFromConfig(awsCfg)
	ecsExecutor := ecsExec.NewECSExecutor(
		ecsClient,
		clusterName,
		getenv("AGENT_IMAGE", ""),
		getenv("ECS_EXEC_ROLE_ARN", ""),
		getenv("ECS_TASK_ROLE_ARN", ""),
		strings.Split(getenv("ECS_SUBNETS", ""), ","),
		strings.Split(getenv("ECS_SECURITY_GROUPS", ""), ","),
		awsRegion,
		secretArn,
		getenv("CONTROLLER_URL", ""),
	)

	var k8sExec orchestrator.Executor

	k8sCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("[INFO] Not running in cluster, k8s disabled: %v", err)
	} else {
		k8sClient, err := kubernetes.NewForConfig(k8sCfg)
		if err != nil {
			log.Printf("[WARN] k8s client create failed, k8s disabled: %v", err)
		} else {
			k8sConfigPath := getenv("K8S_CONFIG_PATH", "")
			var k8sServerConfig *config.K8sServerConfig

			if k8sConfigPath != "" {
				k8sServerConfig, err = config.LoadK8sServerConfig(k8sConfigPath)
				if err != nil {
					log.Fatalf("[ERROR] Failed to load K8s config from %s: %v", k8sConfigPath, err)
				}
				log.Printf("[INFO] Loaded K8s server config from %s", k8sConfigPath)
			} else {
				log.Println("[INFO] K8S_CONFIG_PATH not set, using default K8s settings")
			}

			k8sExec = k8s2.NewK8sExecutor(
				k8sClient,
				getenv("K8S_NAMESPACE", "default"),
				getenv("AGENT_IMAGE", ""),
				getenv("CONTROLLER_URL", ""),
				k8sServerConfig,
			)
		}
	}

	store := state.NewStore()

	orch := orchestrator.New(orchestrator.Deps{
		Store:         store,
		ECS:           ecsExecutor,
		K8S:           k8sExec,
		ControllerURL: getenv("CONTROLLER_URL", ""),
		S3Endpoint:    getenv("S3_ENDPOINT", ""),
		S3Bucket:      getenv("S3_BUCKET", ""),
		S3Region:      getenv("S3_REGION", awsRegion),
		S3PathStyle:   getenv("S3_USE_PATH_STYLE", "false") == "true",
	})

	app := fiber.New(fiber.Config{
		StreamRequestBody: true,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       2 * time.Hour,
		DisableKeepalive:  false,
	})
	app.Use(recover.New())

	routes.Setup(app, routes.Dependencies{
		Orch:  orch,
		Store: store,
	})

	app.Get("/health/live", func(c *fiber.Ctx) error {
		return c.SendString("ok")
	})

	app.Get("/health/ready", func(c *fiber.Ctx) error {
		if !serverReadiness.IsReady() {
			return c.Status(503).SendString("not ready")
		}
		return c.SendString("ready")
	})

	app.Get("/", func(c *fiber.Ctx) error {
		if !serverReadiness.IsReady() {
			return c.Status(503).SendString("build controller is starting...")
		}
		return c.SendString("build controller is running")
	})

	serverReadiness.SetReady()
	log.Println("[main] server is ready to accept requests")

	port := getenv("PORT", "3000")

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Println("[main] listening on port", port)
		if err := app.Listen(":" + port); err != nil {
			log.Printf("[main] fiber listen error: %v", err)
		}
	}()

	sig := <-quit
	log.Printf("[main] received signal %v, initiating graceful shutdown...", sig)

	shutdownTimeout := 30 * time.Second
	if err := app.ShutdownWithTimeout(shutdownTimeout); err != nil {
		log.Printf("[main] graceful shutdown error: %v", err)
	}

	log.Println("[main] server gracefully stopped")
}

// cleanupECSTaskDefinitions 는 CLEANUP_ECS_TASK_DEFINITIONS 환경변수가 true 일 때
// 서버 시작 시 기존 ECS task definition 들을 정리(deregister)합니다.
func cleanupECSTaskDefinitions(ctx context.Context, ecsClient *ecs.Client) error {
	log.Println("[cleanup] Starting ECS task definition cleanup...")

	familyPrefix := getenv("AGENT_TASK_FAMILY", "build-agent")

	log.Printf("[cleanup] Looking for task definitions with family prefix: %s", familyPrefix)

	listFamiliesOut, err := ecsClient.ListTaskDefinitionFamilies(ctx, &ecs.ListTaskDefinitionFamiliesInput{
		FamilyPrefix: &familyPrefix,
		Status:       "ACTIVE",
	})
	if err != nil {
		return fmt.Errorf("list task definition families: %w", err)
	}

	if len(listFamiliesOut.Families) == 0 {
		log.Println("[cleanup] No task definitions found to clean up")
		return nil
	}

	log.Printf("[cleanup] Found %d task definition families to clean up", len(listFamiliesOut.Families))

	totalDeregistered := 0
	for _, family := range listFamiliesOut.Families {
		log.Printf("[cleanup] Cleaning up family: %s", family)

		listTaskDefsOut, err := ecsClient.ListTaskDefinitions(ctx, &ecs.ListTaskDefinitionsInput{
			FamilyPrefix: &family,
			Status:       "ACTIVE",
		})
		if err != nil {
			log.Printf("[cleanup] WARNING: Failed to list task definitions for family %s: %v", family, err)
			continue
		}

		for _, taskDefArn := range listTaskDefsOut.TaskDefinitionArns {
			log.Printf("[cleanup]   Deregistering: %s", taskDefArn)

			_, err := ecsClient.DeregisterTaskDefinition(ctx, &ecs.DeregisterTaskDefinitionInput{
				TaskDefinition: &taskDefArn,
			})
			if err != nil {
				log.Printf("[cleanup]   WARNING: Failed to deregister %s: %v", taskDefArn, err)
				continue
			}

			totalDeregistered++
		}
	}

	log.Printf("[cleanup] ECS task definition cleanup completed: %d task definitions deregistered", totalDeregistered)
	return nil
}

// getenv 는 환경변수를 조회하고, 값이 없으면 기본값을 반환합니다.
func getenv(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}

// RegistryAuth 는 private registry 인증 정보를 담는 구조체입니다.
type RegistryAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// ensureRegistrySecret 는 ECS agent 이미지 pull 에 필요한 private registry 인증 정보를
// AWS Secrets Manager 에 생성하거나 기존 secret ARN 을 반환합니다.
func ensureRegistrySecret(ctx context.Context, sm *secretsmanager.Client) (string, error) {
	secretArn := os.Getenv("AGENT_IMAGE_SECRET_ARN")
	name := os.Getenv("AGENT_IMAGE_SECRET_NAME")
	user := os.Getenv("AGENT_IMAGE_SECRET_USERNAME")
	pass := os.Getenv("AGENT_IMAGE_SECRET_PASSWORD")

	if secretArn != "" {
		log.Println("[secret] using existing secret ARN:", secretArn)
		return secretArn, nil
	}

	if name == "" || user == "" || pass == "" {
		log.Println("[secret] no registry secret provided; skipping")
		return "", nil
	}

	payload, _ := json.Marshal(RegistryAuth{
		Username: user,
		Password: pass,
	})

	out, err := sm.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         aws.String(name),
		SecretString: aws.String(string(payload)),
	})

	if err != nil {
		var existsErr *smt.ResourceExistsException
		if ok := errors.As(err, &existsErr); ok {
			get, err2 := sm.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{
				SecretId: aws.String(name),
			})
			if err2 != nil {
				return "", err2
			}
			log.Printf("[secret] secret already exists: %s", *get.ARN)
			return *get.ARN, nil
		}
		return "", fmt.Errorf("create secret error: %w", err)
	}

	log.Println("[secret] created new secret:", *out.ARN)
	log.Println("[secret] Please export this ARN next time as:")
	log.Printf("export AGENT_IMAGE_SECRET_ARN=%s\n", *out.ARN)

	return *out.ARN, nil
}
