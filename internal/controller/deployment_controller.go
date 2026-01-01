package controller

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type runnableFunc func(context.Context) error

func (f runnableFunc) Start(ctx context.Context) error { return f(ctx) }

// DeploymentReconciler reconciles a Deployment object
type DeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// --- 规则 ConfigMap 位置（固定） ---
	RulesConfigMapNamespace string // 例如 "monitoring"
	RulesConfigMapName      string // 例如 "keda-auto-scale-rules"

	// --- 内存缓存：当前规则（由 ConfigMap 驱动） ---
	rules atomic.Value // *Rules

	// --- 控制“ConfigMap 不存在”的日志只打一次 ---
	cmNotFoundLogged atomic.Bool
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Rules 是从 ConfigMap.Data 解析出来的规则结构
type Rules struct {
	Namespace    string
	NamePrefix   string
	LabelKey     string
	LabelValue   string
	MinReplica   int32
	MaxReplica   int32
	CpuThreshold int32
}

func defaultRules() *Rules {
	return &Rules{
		Namespace:    "international",
		NamePrefix:   "international-",
		LabelKey:     "app.lang",
		LabelValue:   "java",
		MinReplica:   1,
		MaxReplica:   5,
		CpuThreshold: 80,
	}
}

// loadRulesFromConfigMap：解析 cm.Data（缺字段则用默认）
func loadRulesFromConfigMap(cm *corev1.ConfigMap) *Rules {
	r := defaultRules()

	// 允许只覆盖部分字段
	if v := strings.TrimSpace(cm.Data["namespace"]); v != "" {
		r.Namespace = v
	}
	if v := strings.TrimSpace(cm.Data["namePrefix"]); v != "" {
		r.NamePrefix = v
	}
	if v := strings.TrimSpace(cm.Data["labelKey"]); v != "" {
		r.LabelKey = v
	}
	if v := strings.TrimSpace(cm.Data["labelValue"]); v != "" {
		r.LabelValue = v
	}
	if v := strings.TrimSpace(cm.Data["minReplica"]); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			r.MinReplica = int32(i)
		}
	}
	if v := strings.TrimSpace(cm.Data["maxReplica"]); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			r.MaxReplica = int32(i)
		}
	}
	if v := strings.TrimSpace(cm.Data["cpuThreshold"]); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			r.CpuThreshold = int32(i)
		}
	}
	return r
}

func (r *DeploymentReconciler) getRules() *Rules {
	v := r.rules.Load()
	if v == nil {
		// 第一次没有缓存，使用默认
		return defaultRules()
	}
	return v.(*Rules)
}

// syncRules：从 apiserver 获取 ConfigMap 并更新内存规则（被 ConfigMap 的事件触发时调用）
func (r *DeploymentReconciler) syncRules(ctx context.Context, logger logr.Logger) error {
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: r.RulesConfigMapNamespace, Name: r.RulesConfigMapName}

	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			// 只在 startup/第一次发现 NotFound 时打一次
			if r.cmNotFoundLogged.CompareAndSwap(false, true) {
				logger.Info("Rules ConfigMap not found; using default rules",
					"configMap", r.RulesConfigMapNamespace+"/"+r.RulesConfigMapName)
			}
			r.rules.Store(defaultRules())
			return nil
		}
		return err
	}

	// 找到了 ConfigMap：更新内存规则，并允许以后再次打印 NotFound（比如用户删了再建）
	r.cmNotFoundLogged.Store(false)

	newRules := loadRulesFromConfigMap(&cm)
	r.rules.Store(newRules)

	logger.Info("Rules updated from ConfigMap",
		"configMap", r.RulesConfigMapNamespace+"/"+r.RulesConfigMapName,
		"namespace", newRules.Namespace,
		"namePrefix", newRules.NamePrefix,
		"labelKey", newRules.LabelKey,
		"labelValue", newRules.LabelValue,
		"minReplica", newRules.MinReplica,
		"maxReplica", newRules.MaxReplica,
		"cpuThreshold", newRules.CpuThreshold)

	return nil
}

func (r *DeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// 1) 先判断是否是 ConfigMap 事件（同一个 controller watch 了 Deployment + ConfigMap）
	if req.Namespace == r.RulesConfigMapNamespace && req.Name == r.RulesConfigMapName {
		logger.Info("Reconciling rules ConfigMap", "configMap", req.Namespace+"/"+req.Name)
		if err := r.syncRules(ctx, logger); err != nil {
			logger.Error(err, "Failed to sync rules from ConfigMap")
			return ctrl.Result{}, err
		}
		// ConfigMap 变更不会直接创建 ScaledObject，这里返回即可
		return ctrl.Result{}, nil
	}

	// 2) 正常 Deployment reconcile
	var deploy appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deploy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 3) 极早过滤：不匹配直接 return，避免任何额外动作（尤其避免频繁读 CM）
	rules := r.getRules()

	if deploy.Namespace != rules.Namespace {
		return ctrl.Result{}, nil
	}
	if !strings.HasPrefix(deploy.Name, rules.NamePrefix) {
		return ctrl.Result{}, nil
	}
	if deploy.Labels == nil || deploy.Labels[rules.LabelKey] != rules.LabelValue {
		return ctrl.Result{}, nil
	}

	logger.Info("Deployment matched rules",
		"deployment", deploy.Namespace+"/"+deploy.Name,
		"rulesNamespace", rules.Namespace,
		"namePrefix", rules.NamePrefix)

	// 4) 构造 ScaledObject（unstructured）
	soName := deploy.Name + "-scaledobject"

	gvk := schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}
	soKey := client.ObjectKey{Namespace: deploy.Namespace, Name: soName}

	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(gvk)
	desired.SetName(soName)
	desired.SetNamespace(deploy.Namespace)
	desired.SetLabels(map[string]string{
		"autoscaler.keda.sh/managed-by": "keda-so-operator",
	})
	desired.SetOwnerReferences([]metav1.OwnerReference{
		*metav1.NewControllerRef(&deploy, appsv1.SchemeGroupVersion.WithKind("Deployment")),
	})
	desired.Object["spec"] = map[string]interface{}{
		"scaleTargetRef": map[string]interface{}{
			"name": deploy.Name,
		},
		// ⚠️ 这里必须是数字，不要用字符串
		"minReplicaCount": rules.MinReplica,
		"maxReplicaCount": rules.MaxReplica,
		"pollingInterval": int32(15),
		"cooldownPeriod":  int32(200),
		"triggers": []interface{}{
			map[string]interface{}{
				"type": "cpu",
				"metadata": map[string]interface{}{
					"type":  "Utilization",
					"value": strconv.Itoa(int(rules.CpuThreshold)),
				},
			},
		},
	}

	// 5) 幂等 Create/Update：先 Get
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(gvk)

	if err := r.Get(ctx, soKey, current); err != nil {
		if apierrors.IsNotFound(err) {
			// 不存在 -> Create
			if err := r.Create(ctx, desired); err != nil {
				logger.Error(err, "Failed to create ScaledObject", "scaledObject", soKey.String())
				return ctrl.Result{}, err
			}
			logger.Info("ScaledObject created", "scaledObject", soKey.String())
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get ScaledObject", "scaledObject", soKey.String())
		return ctrl.Result{}, err
	}

	// 6) 存在 -> Update（保留 resourceVersion）
	desired.SetResourceVersion(current.GetResourceVersion())
	// 也可以保留 annotations 等，这里按需合并；目前简单覆盖 spec/labels/ownerrefs
	if err := r.Update(ctx, desired); err != nil {
		logger.Error(err, "Failed to update ScaledObject", "scaledObject", soKey.String())
		return ctrl.Result{}, err
	}
	logger.Info("ScaledObject updated", "scaledObject", soKey.String())

	return ctrl.Result{}, nil
}

func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// 启动时先把 rules 初始化为默认（避免 nil）
	r.rules.Store(defaultRules())

	if err := mgr.Add(runnableFunc(func(ctx context.Context) error {
		logger := ctrl.Log.WithName("rules-init")
		if err := r.syncRules(ctx, logger); err != nil {
			logger.Error(err, "initial sync rules failed; keep using default rules")
			return nil
		}
		return nil
	})); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				if obj.GetNamespace() == r.RulesConfigMapNamespace && obj.GetName() == r.RulesConfigMapName {
					return []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(obj)}}
				}
				return nil
			}),
		).
		Named("deployment").
		Complete(r)
}
