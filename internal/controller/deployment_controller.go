/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// DeploymentReconciler reconciles a Deployment object
type DeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config *rest.Config
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
func (r *DeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	var deploy appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deploy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.Info("Reconciling Deployment", "name", deploy.Name, "namespace", deploy.Namespace)
	// 规则匹配

	// ----------------------
	// 读取 ConfigMap 规则
	// ConfigMap 名称: "keda-auto-scale-rules"，存储 key/value
	// 例如:
	//   namespace=international
	//   namePrefix=international-
	//   labelKey=app.lang
	//   labelValue=java
	//   minReplica=1
	//   maxReplica=5
	//   cpuThreshold=80
	// ----------------------
	ruleMap := make(map[string]string)
	//可以在集群修改规则，无需重建镜像
	//如果 ConfigMap 不存在，则使用默认规则
	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Name: "keda-auto-scale-rules", Namespace: "monitoring"}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("没有对于的ConfigMap, 将使用默认规则")
			ruleMap["namespace"] = "international"
			ruleMap["namePrefix"] = "international-"
			ruleMap["labelKey"] = "app.lang"
			ruleMap["labelValue"] = "java"
			ruleMap["minReplica"] = "1"
			ruleMap["maxReplica"] = "5"
			ruleMap["cpuThreshold"] = "80"
		} else {
			logger.Error(err, "获取ConfigMap失败，无法读取规则")
			return ctrl.Result{}, err
		}
	} else {
		ruleMap = cm.Data
	}

	// ----------------------
	// 匹配 Deployment 是否符合规则
	// ----------------------
	if deploy.Namespace != ruleMap["namespace"] {
		return ctrl.Result{}, nil
	}
	if !strings.HasPrefix(deploy.Name, ruleMap["namePrefix"]) {
		return ctrl.Result{}, nil
	}
	if deploy.Labels[ruleMap["labelKey"]] != ruleMap["labelValue"] {
		return ctrl.Result{}, nil
	}

	logger.Info("Deployment matches scaling rules, processing ScaledObject", "deployment", deploy.Name)

	soName := deploy.Name + "-scaledobject"
	so := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "keda.sh/v1alpha1",
			"kind":       "ScaledObject",
			"metadata": map[string]interface{}{
				"name":      soName,
				"namespace": deploy.Namespace,
				"labels": map[string]interface{}{
					"autoscaler.keda.sh/managed-by": "keda-so-operator",
				},
			},
			"spec": map[string]interface{}{
				"scaleTargetRef": map[string]interface{}{
					"name": deploy.Name,
				},
				"minReplicaCount": ruleMap["minReplica"],
				"maxReplicaCount": ruleMap["maxReplica"],
				"pollingInterval": 15,
				"cooldownPeriod":  200,
				"triggers": []interface{}{
					map[string]interface{}{
						"type": "cpu",
						"metadata": map[string]interface{}{
							"type":  "Utilization",
							"value": ruleMap["cpuThreshold"],
						},
					},
				},
			},
		},
	}

	// ----------------------
	// 设置 ownerReference 实现自动 GC
	// 当 Deployment 被删除时，ScaledObject 也会被自动删除
	// ----------------------
	so.SetOwnerReferences([]metav1.OwnerReference{
		*metav1.NewControllerRef(&deploy, appsv1.SchemeGroupVersion.WithKind("Deployment")),
	})

	// 设置 GroupVersionKind，client 才能识别 CRD 类型
	so.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "keda.sh",
		Version: "v1alpha1",
		Kind:    "ScaledObject",
	})

	gvr := schema.GroupVersionResource{
		Group:    "keda.sh",
		Version:  "v1alpha1",
		Resource: "scaledobjects",
	}

	// ----------------------
	// 幂等 Create/Update
	// ----------------------
	dynClient, err := dynamic.NewForConfig(r.Config)
	if err != nil {
		logger.Error(err, "Failed to create dynamic client")
		return ctrl.Result{}, err
	}

	_, err = dynClient.Resource(gvr).Namespace(deploy.Namespace).Get(ctx, soName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			//如果不存在对应资源，则创建
			_, err = dynClient.Resource(gvr).Namespace(deploy.Namespace).Create(ctx, so, metav1.CreateOptions{})
			if err != nil {
				logger.Error(err, "创建ScaledObject失败")
				return ctrl.Result{}, err
			}
			logger.Info("ScaledObject 被创建", "name", soName)
		} else {
			logger.Error(err, "获取ScaledObject失败")
			return ctrl.Result{}, err
		}
	} else {
		_, err = dynClient.Resource(gvr).Namespace(deploy.Namespace).Update(ctx, so, metav1.UpdateOptions{})
		if err != nil {
			logger.Error(err, "更新ScaledObject失败")
			return ctrl.Result{}, err
		}
		logger.Info("ScaledObject被更新", "name", soName)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		Named("deployment").
		Complete(r)
}
