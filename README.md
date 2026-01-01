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
