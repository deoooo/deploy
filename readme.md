### 项目简述
通过简单的配置，可以快速触发 Jenkins 上的构建任务。并输出结果到控制台。

### 使用说明

#### 1. 下载脚本 & 安装脚本


#### 2. 配置文件

在用户主目录下创建一个名为 `deploy_config.yaml` 的配置文件，内容如下：

```yaml
jenkins_url: "http://your-jenkins-url"
username: "your-username"
api_token: "your-api-token"
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
```

#### 3. 使用方式

使用以下命令运行项目：

```sh
deploy <env-name>
```

其中 `<env-name>` 是你在配置文件中定义的环境名称。