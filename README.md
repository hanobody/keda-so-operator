# keda-so-operator

## 项目简介

`keda-so-operator` 是一个基于 **Kubebuilder** 开发的 Kubernetes Operator，用于 **自动为符合规则的 Deployment 创建和维护 KEDA ScaledObject**。

该项目的目标是：

- ✅ 通过统一规则自动管理 KEDA 扩缩容
- ✅ 减少人工维护 ScaledObject 的成本和错误
- ✅ 支持动态规则更新（ConfigMap 驱动）
- ✅ 与 KEDA 原生机制完全兼容
- ✅ 支持多命名空间、多业务场景

---

## 核心能力

- 根据 Deployment 的名称前缀 + Label 自动创建 ScaledObject
- 支持 CPU / Memory 扩缩容
- ConfigMap 驱动配置（无需重启即可生效）
- 自动清理不再匹配的 ScaledObject
- 与 Deployment 生命周期绑定（OwnerReference）
- 支持多 Namespace 监听（减少全集群扫描）

---

## 架构概览

```text
+---------------------+
|   Deployment        |
|  (业务应用)         |
+----------+----------+
           |
           v
+----------------------------+
| keda-so-operator           |
|                            |
| - 监听 Deployment          |
| - 读取 ConfigMap 规则      |
| - 创建 / 更新 ScaledObject |
+-------------+--------------+
              |
              v
+----------------------------+
|        KEDA Operator       |
|   (生成 HPA，驱动伸缩)     |
+-------------+--------------+
              |
              v
+----------------------------+
|        Kubernetes HPA      |
+----------------------------+

## 使用方法

部署方法略.....
配置方法：

修改keda-so-operator-rules.yaml

apiVersion: v1
kind: ConfigMap
metadata:
  name: rules
  namespace: monitoring
data:

#修改命名空间必须重启 operator 生效
#monitoring 是cm所在命名空间 cm在哪就写哪
#area global是部署应用的命名空间
  rules.yaml: |
    namespaces:
      - area
      - global
      - monitoring
    # 必须至少指定一个命名空间，否则规则配置将被视为无效
    #匹配 international-  开头的deployment
    namePrefix: international-
    #该服务label 必须有  kedascan: "true"
    labelKey: kedascan
    labelValue: "true"
#创建 ScaledObject 的 spec 模板 可以选择CPU或内存等触发器
#修改这些参数无需重启operator  会自动同步到目前由operator管理的服务
#删除deployment 或者  删除kedascan标签   scaledobject  会自动级联删除
 scaledobject-spec.yaml: |
    minReplicaCount: 1
    maxReplicaCount: 5
    pollingInterval: 15
    cooldownPeriod: 200
    triggers:
      - type: cpu
        metricType: Utilization
        metadata:
          value: "90"
      - type: memory
        metricType: Utilization
        metadata:
          value: "70"
## 开发者快速指南

如果你在 VS Code 等本地环境中同步和验证代码，可以参考以下流程：

1. 拉取远程最新代码：`git pull`。
2. 查看本地改动：使用 VS Code 源码管理面板或运行 `git status`、`git diff`。
3. 本地验证：至少运行一次核心测试，例如 `go test ./internal/controller -run TestParseRulesFromConfigMap -count=1 -short`。
4. 提交并推送：`git commit -am "<描述>"` 后执行 `git push origin <分支名>`。
5. 创建合并请求：在远程仓库填写改动摘要和测试结果，并等待 CI 通过。