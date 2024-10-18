package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/bndr/gojenkins"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config represents the structure of the YAML configuration file
type Project struct {
	Name string `yaml:"name"`
	Envs []Env  `yaml:"envs"`
}

type Env struct {
	Name    string  `yaml:"name"`
	JobName string  `yaml:"job_name"`
	Params  []Param `yaml:"params,omitempty"`
}

type Param struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type Config struct {
	JenkinsURL string    `yaml:"jenkins_url"`
	Username   string    `yaml:"username"`
	APIToken   string    `yaml:"api_token"`
	Projects   []Project `yaml:"projects"`
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

	BuildJenkinsJob(jobName, params, err, jenkins, ctx)
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

func BuildJenkinsJob(jobName string, params map[string]string, err error, jenkins *gojenkins.Jenkins, ctx context.Context) {
	paramJSON, _ := json.Marshal(params)
	fmt.Printf("Triggering build job: %s params: %s\n", jobName, paramJSON)

	job, err := jenkins.GetJob(ctx, jobName)
	if err != nil {
		log.Fatalf("Failed to get job: %s", err)
	}

	queueID, err := job.InvokeSimple(ctx, params)

	if err != nil {
		log.Fatalf("Failed to trigger build: %s", err)
	}

	fmt.Println("Triggered building", queueID)

	//build, err := job.GetBuild(ctx, queueID)
	build, err := jenkins.GetBuildFromQueueID(ctx, queueID)
	if err != nil {
		log.Fatalf("Failed to get build: %s", err)
	}

	// Wait for build to finish
	for build.IsRunning(ctx) {
		time.Sleep(300 * time.Millisecond)
		_, err := build.Poll(ctx)
		if err != nil {
			log.Fatalf("Failed to poll build: %s", err)
		}
	}

	if build.IsGood(ctx) {
		fmt.Println("Build succeeded")
	} else {
		fmt.Println("")
		fmt.Println("=============Build Failed Log=============")
		fmt.Print(build.GetConsoleOutput(ctx))
		fmt.Println("=============Build Failed Log=============")
		fmt.Println("")
		log.Fatalf("Build failed: %s", build.GetResult())
	}
}
