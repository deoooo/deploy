package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bndr/gojenkins"
	"gopkg.in/yaml.v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Config represents the structure of the YAML configuration file
type Project struct {
	Name string `yaml:"name"`
	Envs []Env  `yaml:"envs"`
}

type Env struct {
	Name    string    `yaml:"name"`
	JobName string    `yaml:"job_name"`
	Params  []Param   `yaml:"params,omitempty"`
	K8s     K8sConfig `yaml:"k8s,omitempty"`
}

type K8sConfig struct {
	Namespace  string `yaml:"namespace"`
	Deployment string `yaml:"deployment"`
	ConfigPath string `yaml:"config_path,omitempty"`
}

type GlobalK8sConfig struct {
	ConfigPath string `yaml:"config_path"`
}

type Param struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type Config struct {
	JenkinsURL string          `yaml:"jenkins_url"`
	Username   string          `yaml:"username"`
	APIToken   string          `yaml:"api_token"`
	K8s        GlobalK8sConfig `yaml:"k8s"`
	Projects   []Project       `yaml:"projects"`
}

// LoadConfig loads the configuration from the specified YAML file
func LoadConfig(filePath string) (*Config, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}

func main() {
	execPath, err := os.Getwd()
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	// 获取目录的名称作为项目名称
	projectName := filepath.Base(execPath)

	// 获取环境
	envName := os.Args[1]

	fmt.Printf("project: %s, env: %s\n", projectName, envName)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Println("Error getting user home directory:", err)
		return
	}

	configFilePath := filepath.Join(homeDir, "deploy_config.yaml")

	config, err := LoadConfig(configFilePath)
	if err != nil {
		log.Fatalf("Failed to load config: %s", err)
	}

	// Find the project in the configuration
	var p Project
	for _, project := range config.Projects {
		if project.Name == projectName {
			p = project
			break
		}
	}
	if p.Name == "" {
		log.Fatalf("Project not found in config: %s", projectName)
	}

	var env Env
	for _, e := range p.Envs {
		if e.Name == envName {
			env = e
			break
		}
	}
	if env.Name == "" {
		log.Fatalf("Env not found in config: %s", envName)
	}

	// build job name
	jobName := env.JobName
	params := parseParams(env)

	ctx := context.Background()
	jenkins := gojenkins.CreateJenkins(nil, config.JenkinsURL, config.Username, config.APIToken)
	_, err = jenkins.Init(ctx)
	if err != nil {
		log.Fatalf("Failed to connect to Jenkins: %s", err)
	}

	fmt.Println("Successfully connected to Jenkins")

	BuildJenkinsJob(jobName, params, err, jenkins, ctx, env, config)
}

func parseParams(env Env) map[string]string {
	params := make(map[string]string)
	for _, param := range env.Params {
		if param.Value == "$branch" {
			// 读取当前目录的git分支名称
			params[param.Name] = getBranchName()
		} else {
			params[param.Name] = param.Value
		}
	}
	return params
}

func getBranchName() string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")

	// 捕获命令的输出
	var out bytes.Buffer
	cmd.Stdout = &out

	// 运行命令
	err := cmd.Run()
	if err != nil {
		log.Fatalf("Failed to get branch: %s", err)
	}
	// 获取输出并去掉尾部的换行符
	branchName := strings.TrimSpace(out.String())
	return branchName
}

func BuildJenkinsJob(jobName string, params map[string]string, err error, jenkins *gojenkins.Jenkins, ctx context.Context, env Env, config *Config) {
	startTime := time.Now().Local()
	fmt.Printf("[%s] Starting Jenkins build job: %s\n", startTime.Format("2006-01-02 15:04:05"), jobName)

	paramJSON, _ := json.Marshal(params)
	fmt.Printf("[%s] Build parameters: %s\n", time.Now().Local().Format("2006-01-02 15:04:05"), paramJSON)

	job, err := jenkins.GetJob(ctx, jobName)
	if err != nil {
		log.Fatalf("Failed to get job: %s", err)
	}

	queueID, err := job.InvokeSimple(ctx, params)
	if err != nil {
		log.Fatalf("Failed to trigger build: %s", err)
	}

	fmt.Printf("[%s] Build triggered with queue ID: %d\n", time.Now().Local().Format("2006-01-02 15:04:05"), queueID)

	build, err := jenkins.GetBuildFromQueueID(ctx, queueID)
	if err != nil {
		log.Fatalf("Failed to get build: %s", err)
	}

	buildStartTime := time.Now()
	lastLogLength := 0
	shouldShowLogs := false

	// Wait for build to finish
	for build.IsRunning(ctx) {
		time.Sleep(300 * time.Millisecond)
		_, err := build.Poll(ctx)
		if err != nil {
			log.Fatalf("Failed to poll build: %s", err)
		}

		// Check if 30 seconds have passed
		if !shouldShowLogs && time.Since(buildStartTime) > 30*time.Second {
			shouldShowLogs = true
			fmt.Printf("\n[%s] Build is taking longer than 30 seconds. Showing real-time logs:\n", time.Now().Local().Format("2006-01-02 15:04:05"))
		}

		// If we should show logs, get and display new content
		if shouldShowLogs {
			logs := build.GetConsoleOutput(ctx)
			if len(logs) > lastLogLength {
				newLogs := logs[lastLogLength:]
				fmt.Print(newLogs)
				lastLogLength = len(logs)
			}
		}
	}

	if build.IsGood(ctx) {
		endTime := time.Now().Local()
		jenkinsDuration := endTime.Sub(startTime)
		fmt.Printf("[%s] Jenkins build completed successfully! Jenkins execution time: %v\n",
			endTime.Format("2006-01-02 15:04:05"), jenkinsDuration)

		// 如果构建成功，监控pod更新
		configPath := env.K8s.ConfigPath
		if configPath == "" {
			configPath = config.K8s.ConfigPath
		}
		if err := monitorPodRollout(ctx, env.K8s.Namespace, env.K8s.Deployment, configPath); err != nil {
			log.Fatalf("Failed to monitor pod rollout: %s", err)
		}
	} else {
		endTime := time.Now().Local()
		jenkinsDuration := endTime.Sub(startTime)
		fmt.Printf("\n[%s] =============Build Failed Log=============\n", endTime.Format("2006-01-02 15:04:05"))
		fmt.Print(build.GetConsoleOutput(ctx))
		fmt.Printf("\n[%s] =============Build Failed Log=============\n", endTime.Format("2006-01-02 15:04:05"))
		fmt.Printf("[%s] Jenkins build failed after %v\n", endTime.Format("2006-01-02 15:04:05"), jenkinsDuration)
		log.Fatalf("Build failed: %s", build.GetResult())
	}
}

func monitorPodRollout(ctx context.Context, namespace, deploymentName string, configPath string) error {
	startTime := time.Now().Local()
	fmt.Printf("[%s] Starting pod rollout monitoring for deployment %s in namespace %s...\n",
		startTime.Format("2006-01-02 15:04:05"), deploymentName, namespace)

	var k8sConfig *rest.Config
	var err error

	// 如果提供了配置文件路径，使用指定的配置文件
	if configPath != "" {
		// 展开 ~ 到用户主目录
		if strings.HasPrefix(configPath, "~/") {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("failed to get user home directory: %v", err)
			}
			configPath = filepath.Join(homeDir, configPath[2:])
		}

		k8sConfig, err = clientcmd.BuildConfigFromFlags("", configPath)
		if err != nil {
			return fmt.Errorf("failed to build config from flags: %v", err)
		}
	} else {
		// 尝试使用集群内配置
		k8sConfig, err = rest.InClusterConfig()
		if err != nil {
			// 如果集群内配置失败，尝试使用默认的 kubeconfig
			k8sConfig, err = clientcmd.BuildConfigFromFlags("", filepath.Join(os.Getenv("HOME"), ".kube", "config"))
			if err != nil {
				return fmt.Errorf("failed to get k8s config: %v", err)
			}
		}
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %v", err)
	}

	// 获取当前部署的版本
	deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get deployment: %v", err)
	}

	// 等待新的pod准备就绪
	for {
		time.Sleep(3 * time.Second)

		// 获取最新的部署状态
		deployment, err = clientset.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get deployment: %v", err)
		}

		// 检查部署是否完成
		if deployment.Status.ReadyReplicas == *deployment.Spec.Replicas {
			endTime := time.Now().Local()
			rolloutDuration := endTime.Sub(startTime)
			fmt.Printf("[%s] K8s rollout completed successfully! Rollout time: %v\n",
				endTime.Format("2006-01-02 15:04:05"), rolloutDuration)
			return nil
		}

		// 检查是否有错误
		if deployment.Status.UnavailableReplicas > 0 {
			endTime := time.Now().Local()
			rolloutDuration := endTime.Sub(startTime)
			return fmt.Errorf("[%s] K8s rollout failed after %v",
				endTime.Format("2006-01-02 15:04:05"), rolloutDuration)
		}

		fmt.Printf("[%s] Waiting for pod rollout to complete... (Ready: %d/%d)\n",
			time.Now().Local().Format("2006-01-02 15:04:05"),
			deployment.Status.ReadyReplicas, *deployment.Spec.Replicas)
	}
}
