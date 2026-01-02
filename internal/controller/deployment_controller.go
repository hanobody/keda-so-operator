package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/go-logr/logr"
	"github.com/hanobody/keda-so-operator/internal/notify"
	"go.yaml.in/yaml/v2"
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

type DeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	RulesConfigMapNamespace string
	RulesConfigMapName      string

	rules atomic.Value

	cmNotFoundLogged atomic.Bool
	cmParseErrLogged atomic.Bool
	Notifier         *notify.TelegramNotifier
}

func scaledObjectReadyState(so *unstructured.Unstructured) string {
	status, ok := so.Object["status"].(map[string]interface{})
	if !ok || status == nil {
		return "Unknown"
	}
	conds, ok := status["conditions"].([]interface{})
	if !ok || len(conds) == 0 {
		return "Unknown"
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "Ready" {
			continue
		}
		if s, _ := m["status"].(string); strings.TrimSpace(s) != "" {
			return s
		}
	}
	return "Unknown"
}
func (r *DeploymentReconciler) notifyDeployment(ctx context.Context, action, ns, name, detail string) {
	if r.Notifier == nil || !r.Notifier.Enabled() {
		return
	}
	msg := fmt.Sprintf("KEDA SO Operator %s\nnamespace: %s\ndeployment: %s\n%s",
		action, ns, name, detail,
	)
	if err := r.Notifier.Send(ctx, msg); err != nil {
		ctrl.Log.WithName("telegram").Error(err, "send telegram failed")
	}
}
func (r *DeploymentReconciler) notifyScaledObject(ctx context.Context, action, ns, soName, ready string) {
	if r.Notifier == nil || !r.Notifier.Enabled() {
		return
	}
	msg := fmt.Sprintf("KEDA ScaledObject %s\nnamespace: %s\nname: %s\nready: %s",
		action, ns, soName, ready,
	)
	if err := r.Notifier.Send(ctx, msg); err != nil {
		ctrl.Log.WithName("telegram").Error(err, "send telegram failed")
	}
}
func normalizeJSONValue(v interface{}) interface{} {
	switch x := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(x))
		for k, vv := range x {
			out[k] = normalizeJSONValue(vv)
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, len(x))
		for _, it := range x {
			out = append(out, normalizeJSONValue(it))
		}
		return out

	case json.Number:
		if i, err := x.Int64(); err == nil {
			return float64(i)
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()

	case int, int8, int16, int32, int64:
		return float64(reflect.ValueOf(x).Int())

	case uint, uint8, uint16, uint32, uint64:
		return float64(reflect.ValueOf(x).Uint())

	case float32:
		return float64(x)
	case float64:
		return x

	default:
		return v
	}
}
func requiredResourcesFromSpec(spec map[string]interface{}) (needCPU bool, needMemory bool) {
	triggers, ok := spec["triggers"].([]interface{})
	if !ok {
		return false, false
	}

	for _, t := range triggers {
		m, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		switch strings.ToLower(fmt.Sprint(m["type"])) {
		case "cpu":
			needCPU = true
		case "memory":
			needMemory = true
		}
	}
	return
}

func missingRequests(deploy *appsv1.Deployment, needCPU, needMemory bool) []string {

	missing := map[string]bool{}

	for _, c := range deploy.Spec.Template.Spec.Containers {
		req := c.Resources.Requests

		if needCPU {
			if req == nil || req.Cpu().IsZero() {
				missing["cpu"] = true
			}
		}
		if needMemory {
			if req == nil || req.Memory().IsZero() {
				missing["memory"] = true
			}
		}
	}

	out := make([]string, 0, len(missing))
	for k := range missing {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func canonicalJSONBytes(v interface{}) ([]byte, error) {
	nv := normalizeJSONValue(v)
	b, err := json.Marshal(nv) // map key 会排序
	if err != nil {
		return nil, err
	}
	// 再做一次 compact，避免空格差异（通常 marshal 已无空格，但保险）
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func specEqual(currentSpec, desiredSpec interface{}) (bool, error) {
	cb, err := canonicalJSONBytes(currentSpec)
	if err != nil {
		return false, err
	}
	db, err := canonicalJSONBytes(desiredSpec)
	if err != nil {
		return false, err
	}
	return bytes.Equal(cb, db), nil
}
func (r *DeploymentReconciler) getRules() *Rules {
	v := r.rules.Load()
	if v == nil {
		return nil
	}
	return v.(*Rules)
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=keda.sh,resources=scaledobjects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

type Rules struct {
	// rules.yaml
	Namespaces []string `yaml:"namespaces"`
	NamePrefix string   `yaml:"namePrefix"`
	LabelKey   string   `yaml:"labelKey"`
	LabelValue string   `yaml:"labelValue"`

	// scaledobject-spec.yaml 解析后的 spec 模板（map[string]any）
	ScaledObjectSpec map[string]interface{}
}

func parseRulesFromConfigMap(cm *corev1.ConfigMap) (*Rules, error) {
	rulesText := strings.TrimSpace(cm.Data["rules.yaml"])
	if rulesText == "" {
		return nil, fmt.Errorf("missing key data[rules.yaml] in ConfigMap %s/%s", cm.Namespace, cm.Name)
	}

	var r Rules
	if err := yaml.Unmarshal([]byte(rulesText), &r); err != nil {
		return nil, fmt.Errorf("failed to parse rules.yaml: %w", err)
	}

	// namespaces 变更需要重启；这里仍然要求其存在（main.go 也依赖它）
	// 但 controller 端仍校验一下，避免空规则导致误删误建
	cleanedNS := make([]string, 0, len(r.Namespaces))
	for _, ns := range r.Namespaces {
		ns = strings.TrimSpace(ns)
		if ns != "" {
			cleanedNS = append(cleanedNS, ns)
		}
	}
	r.Namespaces = cleanedNS
	if len(r.Namespaces) == 0 {
		return nil, fmt.Errorf("rules.yaml 必须包含至少一个非空的 namespace")
	}
	if strings.TrimSpace(r.NamePrefix) == "" {
		return nil, fmt.Errorf("rules.yaml missing namePrefix")
	}
	if strings.TrimSpace(r.LabelKey) == "" {
		return nil, fmt.Errorf("rules.yaml missing labelKey")
	}
	if strings.TrimSpace(r.LabelValue) == "" {
		return nil, fmt.Errorf("rules.yaml missing labelValue")
	}

	specText := strings.TrimSpace(cm.Data["scaledobject-spec.yaml"])
	if specText == "" {
		return nil, fmt.Errorf("missing key data[scaledobject-spec.yaml] in ConfigMap %s/%s", cm.Namespace, cm.Name)
	}

	spec := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(specText), &spec); err != nil {
		return nil, fmt.Errorf("failed to parse scaledobject-spec.yaml: %w", err)
	}
	r.ScaledObjectSpec = spec
	m, ok := toStringKeyMap(spec).(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("scaledobject-spec.yaml must be a YAML map/object at top level")
	}

	r.ScaledObjectSpec = m
	return &r, nil
}

// 递归把 YAML 的 map[interface{}]interface{} 转为 map[string]interface{}
func toStringKeyMap(in interface{}) interface{} {
	switch v := in.(type) {
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, val := range v {
			out[k] = toStringKeyMap(val)
		}
		return out
	case map[interface{}]interface{}:
		out := map[string]interface{}{}
		for k, val := range v {
			ks := fmt.Sprintf("%v", k)
			out[ks] = toStringKeyMap(val)
		}
		return out
	case []interface{}:
		out := make([]interface{}, 0, len(v))
		for _, it := range v {
			out = append(out, toStringKeyMap(it))
		}
		return out
	default:
		return in
	}
}

func deepCopyMap(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	b, _ := json.Marshal(in)
	out := map[string]interface{}{}
	_ = json.Unmarshal(b, &out)
	return out
}

func (r *DeploymentReconciler) syncRules(ctx context.Context, logger logr.Logger) error {
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: r.RulesConfigMapNamespace, Name: r.RulesConfigMapName}

	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("rules ConfigMap not found: %s/%s", r.RulesConfigMapNamespace, r.RulesConfigMapName)
		}
		return err
	}
	r.cmNotFoundLogged.Store(false)

	newRules, err := parseRulesFromConfigMap(&cm)
	if err != nil {
		// 解析失败：为了安全，清空规则（不做创建/更新/删除），并打日志（只打一次避免刷屏）
		if r.cmParseErrLogged.CompareAndSwap(false, true) {
			logger.Error(err, "Failed to parse rules ConfigMap; controller will be idle until fixed",
				"configMap", r.RulesConfigMapNamespace+"/"+r.RulesConfigMapName)
		}
		r.rules.Store((*Rules)(nil))
		return nil
	}
	r.cmParseErrLogged.Store(false)

	r.rules.Store(newRules)
	logger.Info("Rules updated from ConfigMap",
		"configMap", r.RulesConfigMapNamespace+"/"+r.RulesConfigMapName,
		"namePrefix", newRules.NamePrefix,
		"labelKey", newRules.LabelKey,
		"labelValue", newRules.LabelValue,
		"namespaces", newRules.Namespaces,
	)
	return nil
}
func (r *DeploymentReconciler) deleteManagedScaledObjectIfExists(ctx context.Context, logger logr.Logger, ns, name string) error {
	gvk := schema.GroupVersionKind{Group: "keda.sh", Version: "v1alpha1", Kind: "ScaledObject"}
	soKey := client.ObjectKey{Namespace: ns, Name: name}

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(gvk)

	if err := r.Get(ctx, soKey, current); err != nil {
		return client.IgnoreNotFound(err)
	}

	labels := current.GetLabels()
	if labels == nil || labels["autoscaler.keda.sh/managed-by"] != "keda-so-operator" {
		return nil
	}

	if err := r.Delete(ctx, current); err != nil {
		return err
	}
	logger.Info("ScaledObject deleted (no longer matches rules)", "scaledObject", soKey.String())
	if r.Notifier != nil {
		r.notifyScaledObject(
			ctx,
			"Deleted",
			soKey.Namespace,
			soKey.Name,
			scaledObjectReadyState(current),
		)
	}
	return nil
}
func (r *DeploymentReconciler) matchesRules(deploy *appsv1.Deployment, rules *Rules) bool {
	if rules == nil {
		return false
	}
	if !strings.HasPrefix(deploy.Name, rules.NamePrefix) {
		return false
	}
	if deploy.Labels == nil || deploy.Labels[rules.LabelKey] != rules.LabelValue {
		return false
	}
	// namespace 由 cache 限定 + rules.yaml 声明，这里不再强行等于某个固定 namespace
	// （因为你 rules.yaml 是多个 namespaces）
	return true
}
func (r *DeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// 1) ConfigMap 事件
	if req.Namespace == r.RulesConfigMapNamespace && req.Name == r.RulesConfigMapName {
		logger.Info("Reconciling rules ConfigMap", "configMap", req.Namespace+"/"+req.Name)
		if err := r.syncRules(ctx, logger); err != nil {
			logger.Error(err, "Failed to sync rules from ConfigMap")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 2) Deployment 事件
	var deploy appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deploy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	rules := r.getRules()
	if rules == nil {
		// 无规则/规则解析失败：为了安全不做任何变更（不建不改不删）
		return ctrl.Result{}, nil
	}

	soName := deploy.Name + "-scaledobject"

	// 3) 不匹配：删除已有 SO（仅删除 managed-by 的）
	if !r.matchesRules(&deploy, rules) {
		if err := r.deleteManagedScaledObjectIfExists(ctx, logger, deploy.Namespace, soName); err != nil {
			logger.Error(err, "Failed to delete ScaledObject", "deployment", deploy.Namespace+"/"+deploy.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	needCPU, needMemory := requiredResourcesFromSpec(rules.ScaledObjectSpec)

	missing := missingRequests(&deploy, needCPU, needMemory)
	if len(missing) > 0 {
		msg := fmt.Sprintf(
			"deployment 缺少 Requests 资源配置: %s",
			strings.Join(missing, ", "),
		)

		logger.Info("Skip ScaledObject due to missing requests",
			"deployment", deploy.Namespace+"/"+deploy.Name,
			"missing", missing,
		)

		r.notifyDeployment(
			ctx,
			"Skipped ScaledObject",
			deploy.Namespace,
			deploy.Name,
			msg,
		)

		return ctrl.Result{}, nil
	}
	// 4) 匹配：Create/Update ScaledObject，spec 从模板来
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

	spec := deepCopyMap(rules.ScaledObjectSpec)
	spec = toStringKeyMap(spec).(map[string]interface{})
	// 保证 scaleTargetRef.name 正确
	str, ok := spec["scaleTargetRef"].(map[string]interface{})
	if !ok || str == nil {
		str = map[string]interface{}{}
		spec["scaleTargetRef"] = str
	}
	str["name"] = deploy.Name

	desired.Object["spec"] = spec

	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(gvk)

	if err := r.Get(ctx, soKey, current); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, desired); err != nil {
				logger.Error(err, "Failed to create ScaledObject", "scaledObject", soKey.String())
				return ctrl.Result{}, err
			}
			logger.Info("ScaledObject created", "scaledObject", soKey.String())

			if r.Notifier != nil {
				r.notifyScaledObject(
					ctx,
					"Created",
					soKey.Namespace,
					soKey.Name,
					"创建成功",
				)
			}
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get ScaledObject", "scaledObject", soKey.String())
		return ctrl.Result{}, err
	}

	sameSpec, err := specEqual(current.Object["spec"], desired.Object["spec"])
	if err != nil {
		logger.Error(err, "Failed to compare spec; will proceed to update", "scaledObject", soKey.String())
	} else {
		if sameSpec && reflect.DeepEqual(current.GetLabels(), desired.GetLabels()) {
			return ctrl.Result{}, nil
		}
	}

	desired.SetResourceVersion(current.GetResourceVersion())
	if err := r.Update(ctx, desired); err != nil {
		logger.Error(err, "Failed to update ScaledObject", "scaledObject", soKey.String())
		return ctrl.Result{}, err
	}
	logger.Info("ScaledObject updated", "scaledObject", soKey.String())
	var updated unstructured.Unstructured
	updated.SetGroupVersionKind(gvk)
	ready := "Unknown"
	if err := r.Get(ctx, soKey, &updated); err == nil {
		ready = scaledObjectReadyState(&updated)
	}

	if r.Notifier != nil {
		r.notifyScaledObject(
			ctx,
			"Updated",
			soKey.Namespace,
			soKey.Name,
			ready,
		)
	}
	return ctrl.Result{}, nil
}

func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// 启动时先 sync 一次 rules
	if err := mgr.Add(runnableFunc(func(ctx context.Context) error {
		logger := ctrl.Log.WithName("rules-init")
		if err := r.syncRules(ctx, logger); err != nil {
			logger.Error(err, "initial sync rules failed")
			return err
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
				if obj.GetNamespace() != r.RulesConfigMapNamespace || obj.GetName() != r.RulesConfigMapName {
					return nil
				}

				// 1) 先把 ConfigMap 自己 enqueue（用于触发 syncRules）
				reqs := []reconcile.Request{
					{NamespacedName: client.ObjectKeyFromObject(obj)},
				}

				// 2) 再把 cache 范围内的所有 Deployment enqueue，让它们根据新规则自我 reconcile（更新/删除 SO）
				var dl appsv1.DeploymentList
				if err := r.List(ctx, &dl); err != nil {
					// list 失败就只 enqueue cm，本次规则不会批量生效，但下一次 deployment 变更会生效
					ctrl.Log.WithName("rules-watch").Error(err, "list deployments failed when rules changed")
					return reqs
				}
				for i := range dl.Items {
					d := &dl.Items[i]
					reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(d)})
				}
				return reqs
			}),
		).
		Named("deployment").
		Complete(r)
}
