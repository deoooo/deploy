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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

	// 获取当前部署的revision
	configPath := env.K8s.ConfigPath
	if configPath == "" {
		configPath = config.K8s.ConfigPath
	}

	// 检查部署名称是否为空
	if env.K8s.Namespace == "" || env.K8s.Deployment == "" {
		log.Fatalf("K8s deployment configuration incomplete: namespace=%s, deployment=%s",
			env.K8s.Namespace, env.K8s.Deployment)
	}

	// 获取当前部署的revision和pod列表
	initialRevision, initialPodUIDs, err := getCurrentDeploymentStatus(ctx, env.K8s.Namespace, env.K8s.Deployment, configPath)
	if err != nil {
		log.Fatalf("Failed to get current deployment status: %s", err)
	}
	fmt.Printf("Current deployment revision: %s, found %d pods\n", initialRevision, len(initialPodUIDs))

	var success bool
	success, err = BuildJenkinsJob(jobName, params, err, jenkins, ctx, env, config)
	if !success {
		log.Fatalf("Failed to build Jenkins job: %s", err)
	}

	// 如果构建成功，监控pod更新
	if err := monitorPodRollout(ctx, env.K8s.Namespace, env.K8s.Deployment, configPath, initialRevision, initialPodUIDs); err != nil {
		log.Fatalf("Failed to monitor pod rollout: %s", err)
	}
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

func BuildJenkinsJob(jobName string, params map[string]string, err error, jenkins *gojenkins.Jenkins, ctx context.Context, env Env, config *Config) (bool, error) {
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

		return true, nil
	} else {
		endTime := time.Now().Local()
		jenkinsDuration := endTime.Sub(startTime)
		fmt.Printf("\n[%s] =============Build Failed Log=============\n", endTime.Format("2006-01-02 15:04:05"))
		fmt.Print(build.GetConsoleOutput(ctx))
		fmt.Printf("\n[%s] =============Build Failed Log=============\n", endTime.Format("2006-01-02 15:04:05"))
		fmt.Printf("[%s] Jenkins build failed after %v\n", endTime.Format("2006-01-02 15:04:05"), jenkinsDuration)
		log.Fatalf("Build failed: %s", build.GetResult())
		return false, nil
	}
}

func monitorPodRollout(ctx context.Context, namespace, deploymentName string, configPath string, initialRevision string, initialPodUIDs map[string]bool) error {
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

	// 直接使用传入的初始 revision 和 Pod UID 列表
	fmt.Printf("[%s] Monitoring rollout from revision: %s, found %d initial pods\n",
		time.Now().Local().Format("2006-01-02 15:04:05"), initialRevision, len(initialPodUIDs))

	// 存储最大重试次数和超时
	maxRetries := 120 // 10分钟 (5秒 * 120)
	retries := 0

	// 等待新的pod准备就绪
	for {
		if retries >= maxRetries {
			return fmt.Errorf("rollout timed out after %d attempts", maxRetries)
		}

		time.Sleep(5 * time.Second) // 增加等待时间，让健康检查有足够时间执行
		retries++

		// 获取最新的部署状态
		deployment, err = clientset.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get deployment: %v", err)
		}

		// 获取与部署关联的所有pod
		podList, err := getDeploymentPods(ctx, clientset, namespace, deployment)
		if err != nil {
			return fmt.Errorf("failed to get pods: %v", err)
		}

		// 检查新旧pod状态
		newPods, oldPods := categorizePodsByUID(podList, initialPodUIDs)
		readyNewPods := countReadyAndHealthyPods(newPods)

		// 输出当前状态和健康检查详情
		fmt.Printf("[%s] Pod status: %d/%d new pods ready, %d old pods remaining\n",
			time.Now().Local().Format("2006-01-02 15:04:05"),
			readyNewPods, len(newPods), len(oldPods))

		// 输出任何未就绪新pod的详细状态
		if readyNewPods < len(newPods) {
			for _, pod := range newPods {
				if !isPodReadyAndHealthy(pod) {
					fmt.Printf("[%s] New pod %s not ready: Phase=%s, Ready=%v, ContainerReady=%v\n",
						time.Now().Local().Format("2006-01-02 15:04:05"),
						pod.Name, pod.Status.Phase, isPodReady(pod), areAllContainersReady(pod))

					// 输出健康检查失败的容器信息
					for _, containerStatus := range pod.Status.ContainerStatuses {
						if !containerStatus.Ready {
							state := "Unknown"
							if containerStatus.State.Waiting != nil {
								state = fmt.Sprintf("Waiting: %s (%s)",
									containerStatus.State.Waiting.Reason,
									containerStatus.State.Waiting.Message)
							} else if containerStatus.State.Terminated != nil {
								state = fmt.Sprintf("Terminated: %s (%s)",
									containerStatus.State.Terminated.Reason,
									containerStatus.State.Terminated.Message)
							}
							fmt.Printf("[%s] Container %s not ready: %s, RestartCount=%d\n",
								time.Now().Local().Format("2006-01-02 15:04:05"),
								containerStatus.Name, state, containerStatus.RestartCount)
						}
					}
				}
			}
		}

		// 检查部署是否完成：所有新pod已就绪且没有旧pod
		if readyNewPods == int(*deployment.Spec.Replicas) && len(oldPods) == 0 {
			// 成功后额外等待10秒，确保pod真正稳定
			fmt.Printf("[%s] All pods ready, waiting additional 10 seconds to ensure stability...\n",
				time.Now().Local().Format("2006-01-02 15:04:05"))
			time.Sleep(10 * time.Second)

			// 再次检查所有pod状态
			podList, err = getDeploymentPods(ctx, clientset, namespace, deployment)
			if err != nil {
				return fmt.Errorf("failed to get pods during final check: %v", err)
			}

			newPods, _ = categorizePodsByUID(podList, initialPodUIDs)
			readyNewPods = countReadyAndHealthyPods(newPods)

			if readyNewPods == int(*deployment.Spec.Replicas) {
				endTime := time.Now().Local()
				rolloutDuration := endTime.Sub(startTime)
				fmt.Printf("[%s] K8s rollout completed successfully! Rollout time: %v\n",
					endTime.Format("2006-01-02 15:04:05"), rolloutDuration)
				return nil
			} else {
				fmt.Printf("[%s] Pods became unhealthy during stability check, continuing to monitor\n",
					time.Now().Local().Format("2006-01-02 15:04:05"))
			}
		}

		// 检查是否有错误
		if deployment.Status.UnavailableReplicas > 0 && retries > 10 {
			// 检查是否有异常pod
			errorPods := findErrorPods(newPods)
			if len(errorPods) > 0 {
				for _, pod := range errorPods {
					fmt.Printf("[%s] Problem pod: %s, status: %s, message: %s\n",
						time.Now().Local().Format("2006-01-02 15:04:05"),
						pod.Name, getPodStatus(pod), getPodErrorMessage(pod))
				}
				endTime := time.Now().Local()
				rolloutDuration := endTime.Sub(startTime)
				return fmt.Errorf("[%s] K8s rollout failed after %v - new pods are not becoming ready",
					endTime.Format("2006-01-02 15:04:05"), rolloutDuration)
			}
		}
	}
}

// 从部署中获取修订版本
func getDeploymentRevision(deployment *appsv1.Deployment) string {
	if annotations := deployment.GetAnnotations(); annotations != nil {
		return annotations["deployment.kubernetes.io/revision"]
	}
	return ""
}

// 获取与部署相关联的所有pod
func getDeploymentPods(ctx context.Context, clientset *kubernetes.Clientset, namespace string, deployment *appsv1.Deployment) (*corev1.PodList, error) {
	// 从部署中提取选择器
	deploymentLabels := deployment.Spec.Selector.MatchLabels
	if len(deploymentLabels) == 0 {
		return nil, fmt.Errorf("deployment has no selector labels for pod selection")
	}

	// 构建标签选择器
	var selectorBuilder strings.Builder
	first := true
	for k, v := range deploymentLabels {
		if !first {
			selectorBuilder.WriteString(",")
		}
		selectorBuilder.WriteString(fmt.Sprintf("%s=%s", k, v))
		first = false
	}

	selector := selectorBuilder.String()
	return clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
}

// 将pod分类为新pod和旧pod
func categorizePods(podList *corev1.PodList, initialRevision string) ([]*corev1.Pod, []*corev1.Pod) {
	var newPods, oldPods []*corev1.Pod

	for i := range podList.Items {
		pod := &podList.Items[i]
		podRevision := pod.Annotations["deployment.kubernetes.io/revision"]

		// 检查是否是当前修订版本后创建的pod
		if podRevision > initialRevision {
			newPods = append(newPods, pod)
		} else {
			oldPods = append(oldPods, pod)
		}
	}

	return newPods, oldPods
}

// 新增基于 UID 的分类函数，更准确地标识新旧 Pod
func categorizePodsByUID(podList *corev1.PodList, initialPodUIDs map[string]bool) ([]*corev1.Pod, []*corev1.Pod) {
	var newPods, oldPods []*corev1.Pod

	for i := range podList.Items {
		pod := &podList.Items[i]
		// 如果 Pod UID 在初始列表中，则为旧 Pod
		if initialPodUIDs[string(pod.UID)] {
			oldPods = append(oldPods, pod)
		} else {
			newPods = append(newPods, pod)
		}
	}

	return newPods, oldPods
}

// 计算准备就绪且健康的pod数量
func countReadyAndHealthyPods(pods []*corev1.Pod) int {
	readyCount := 0

	for _, pod := range pods {
		if isPodReadyAndHealthy(pod) {
			readyCount++
		}
	}

	return readyCount
}

// 检查pod是否准备就绪且健康
func isPodReadyAndHealthy(pod *corev1.Pod) bool {
	// 检查pod是否处于Running状态
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	// 检查所有pod条件
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status != corev1.ConditionTrue {
			return false
		}
	}

	// 检查所有容器状态
	for _, containerStatus := range pod.Status.ContainerStatuses {
		// 检查容器是否运行中
		if !containerStatus.Ready {
			return false
		}

		// 检查容器是否频繁重启 (可能是由于liveness probe失败)
		if containerStatus.RestartCount > 3 && timeFromLastRestart(containerStatus) < 60 {
			return false
		}

		// 检查容器是否处于等待状态(如CrashLoopBackOff, ImagePullBackOff等)
		if containerStatus.State.Waiting != nil {
			return false
		}
	}

	return true
}

// 计算从容器最后一次重启到现在的秒数
func timeFromLastRestart(containerStatus corev1.ContainerStatus) int64 {
	if containerStatus.LastTerminationState.Terminated != nil &&
		!containerStatus.LastTerminationState.Terminated.FinishedAt.IsZero() {
		now := time.Now()
		lastRestartTime := containerStatus.LastTerminationState.Terminated.FinishedAt.Time
		return int64(now.Sub(lastRestartTime).Seconds())
	}
	return 1000 // 如果没有重启记录，返回一个较大的值
}

// 查找错误的pod
func findErrorPods(pods []*corev1.Pod) []*corev1.Pod {
	var errorPods []*corev1.Pod

	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodFailed ||
			pod.Status.Phase == corev1.PodUnknown ||
			hasCrashLoopBackOff(pod) {
			errorPods = append(errorPods, pod)
		}
	}

	return errorPods
}

// 检查pod是否处于CrashLoopBackOff状态
func hasCrashLoopBackOff(pod *corev1.Pod) bool {
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.State.Waiting != nil &&
			containerStatus.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	return false
}

// 获取pod状态
func getPodStatus(pod *corev1.Pod) string {
	return string(pod.Status.Phase)
}

// 获取pod错误消息
func getPodErrorMessage(pod *corev1.Pod) string {
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.State.Waiting != nil && containerStatus.State.Waiting.Message != "" {
			return containerStatus.State.Waiting.Message
		}
		if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.Message != "" {
			return containerStatus.State.Terminated.Message
		}
	}
	return "No error message found"
}

// getCurrentDeploymentStatus 获取当前部署的revision和pod信息
func getCurrentDeploymentStatus(ctx context.Context, namespace, deploymentName, configPath string) (string, map[string]bool, error) {
	var k8sConfig *rest.Config
	var err error

	// 如果提供了配置文件路径，使用指定的配置文件
	if configPath != "" {
		// 展开 ~ 到用户主目录
		if strings.HasPrefix(configPath, "~/") {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return "", nil, fmt.Errorf("failed to get user home directory: %v", err)
			}
			configPath = filepath.Join(homeDir, configPath[2:])
		}

		k8sConfig, err = clientcmd.BuildConfigFromFlags("", configPath)
		if err != nil {
			return "", nil, fmt.Errorf("failed to build config from flags: %v", err)
		}
	} else {
		// 尝试使用集群内配置
		k8sConfig, err = rest.InClusterConfig()
		if err != nil {
			// 如果集群内配置失败，尝试使用默认的 kubeconfig
			k8sConfig, err = clientcmd.BuildConfigFromFlags("", filepath.Join(os.Getenv("HOME"), ".kube", "config"))
			if err != nil {
				return "", nil, fmt.Errorf("failed to get k8s config: %v", err)
			}
		}
	}

	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create kubernetes client: %v", err)
	}

	// 获取当前部署信息
	deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("failed to get deployment: %v", err)
	}

	// 获取当前revision
	initialRevision := getDeploymentRevision(deployment)
	if initialRevision == "" {
		return "", nil, fmt.Errorf("unable to determine deployment revision")
	}

	// 获取与部署关联的所有初始 pod
	initialPodList, err := getDeploymentPods(ctx, clientset, namespace, deployment)
	if err != nil {
		return "", nil, fmt.Errorf("failed to get initial pods: %v", err)
	}

	// 保存初始 Pod 的 UID 列表作为旧 Pod 标识
	initialPodUIDs := make(map[string]bool)
	for i := range initialPodList.Items {
		pod := &initialPodList.Items[i]
		initialPodUIDs[string(pod.UID)] = true
	}

	return initialRevision, initialPodUIDs, nil
}

// isPodReady 检查pod是否处于Ready状态
func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// areAllContainersReady 检查所有容器是否Ready
func areAllContainersReady(pod *corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}

	for _, containerStatus := range pod.Status.ContainerStatuses {
		if !containerStatus.Ready {
			return false
		}
	}
	return true
}
