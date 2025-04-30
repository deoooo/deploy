### 项目简述
通过简单的配置，可以快速触发 Jenkins 上的构建任务。并输出结果到控制台。在Jenkins构建成功后，会自动监控Kubernetes pod的滚动更新状态。

### 使用说明

#### 1. 下载脚本 & 安装脚本
下载解压到`/usr/local/bin`目录

#### 2. 配置文件

在用户主目录下创建一个名为 `deploy_config.yaml` 的配置文件，内容如下：

```yaml
jenkins_url: "http://your-jenkins-url"
username: "your-username"
api_token: "your-api-token"
k8s:
  config_path: "~/.kube/config"  # Global k8s config path
projects:
  - name: "your-project-name"
    envs:
      - name: "your-env-name"
        job_name: "your-job-name"
        params:
          - name: "param1"
            value: "value1"
          - name: "param2"
            value: "$branch"
        k8s:
          namespace: "your-namespace"
          deployment: "your-deployment-name"
          config_path: "~/.kube/custom-config"  # Optional: Project specific k8s config path
```

#### 3. 使用方式

使用以下命令运行项目：

```sh
deploy <env-name>
```

其中 `<env-name>` 是你在配置文件中定义的环境名称。

#### 4. 功能说明

- 触发Jenkins构建任务
- 实时显示构建日志
- 构建成功后自动监控Kubernetes pod的滚动更新
- 等待pod更新完成并输出成功信息