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

package controllers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	hubblev1 "github.com/Trump-last/k8sOperator/api/v1"
	"github.com/google/uuid"
)

// HubbleClusterReconciler reconciles a HubbleCluster object
type HubbleClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=hubble.example.com,resources=hubbleclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=hubble.example.com,resources=hubbleclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=hubble.example.com,resources=hubbleclusters/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the HubbleCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.4/pkg/reconcile
func (r *HubbleClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	// 获取HubbleCluster对象
	cluster := &hubblev1.HubbleCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// 处理删除逻辑
	if !cluster.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.cleanUpResources(ctx, cluster)
	}
	// 添加finalizer
	if !controllerutil.ContainsFinalizer(cluster, "hubble.example.com/finalizer") {
		controllerutil.AddFinalizer(cluster, "hubble.example.com/finalizer")
		if err := r.Update(ctx, cluster); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 初始化uuid池与当前版本
	if len(cluster.Status.ActiveUUIDs) == 0 {
		cluster.Status.ActiveUUIDs = generateUUID(int(cluster.Spec.Replicas))
		cluster.Status.CurrentVersion = cluster.Spec.Image
		if err := r.Status().Update(ctx, cluster); err != nil {
			return ctrl.Result{Requeue: true}, err
		}
	}

	// 同步pod状态，在第一次部署时，确保每个uuid都创建一个pod
	existingPods, err := r.findManagedPods(ctx, cluster)
	if err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}
	if err := r.syncPods(ctx, cluster, existingPods); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to sync pods: %w", err)
	}

	// Step 6: 滚动升级检查
	if cluster.Spec.Image != cluster.Status.CurrentVersion {
		return r.rollingUpgrade(ctx, cluster, existingPods)
	}

	return ctrl.Result{}, nil
}

// 子方法

// uuid池生成
func generateUUID(count int) []string {
	var uuids []string
	for i := 0; i < count; i++ {
		uuids = append(uuids, uuid.NewString())
	}
	return uuids
}

// 清理所有关联的pod
func (r *HubbleClusterReconciler) cleanUpResources(ctx context.Context, cluster *hubblev1.HubbleCluster) error {
	pods, err := r.findManagedPods(ctx, cluster)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if err := r.Delete(ctx, &pod); err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(cluster, "hubble.example.com/finalizer")
	return r.Update(ctx, cluster)
}

// 查找当前集群管理的 Pod
func (r *HubbleClusterReconciler) findManagedPods(ctx context.Context, cluster *hubblev1.HubbleCluster) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(cluster.Namespace),
		client.MatchingLabels{"hubble-cluster": cluster.Name}); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// 同步pod状态，确保每个uuid都有一个pod
func (r *HubbleClusterReconciler) syncPods(
	ctx context.Context,
	cluster *hubblev1.HubbleCluster,
	existingPods []corev1.Pod,
) error {
	existingUuids := make(map[string]struct{})
	for _, pod := range existingPods {
		existingUuids[pod.Labels["hubble-uuid"]] = struct{}{}
	}
	// 创建缺失的uuid
	for _, uuid := range cluster.Status.ActiveUUIDs {
		if _, ok := existingUuids[uuid]; !ok {
			pod := r.buildPod(cluster, uuid)
			if err := controllerutil.SetControllerReference(cluster, pod, r.Scheme); err != nil {
				return err
			}
			if err := r.Create(ctx, pod); err != nil {
				return fmt.Errorf("failed to create pod: %w", err)
			}
		}
	}
	return nil
}

// 滚动升级（核心代码）
func (r *HubbleClusterReconciler) rollingUpgrade(
	ctx context.Context,
	cluster *hubblev1.HubbleCluster,
	existingPods []corev1.Pod,
) (ctrl.Result, error) {
	for _, pod := range existingPods {
		if pod.Labels["hubble-version"] == cluster.Spec.Image {
			continue // 版本一致，不需要升级
		}
		// 步骤一，先发给old pod一个停止接收消息的信号,让它不要处理新的消息
		if ok, err := r.sendStopSignal(&pod); !ok || err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
		// 步骤二，创建一个新的pod，使用相同的uuid
		newPod := r.buildPod(cluster, pod.Labels["hubble-uuid"])
		if err := controllerutil.SetControllerReference(cluster, newPod, r.Scheme); err != nil {
			return ctrl.Result{}, err
		} // 要绑定controller
		if err := r.Create(ctx, newPod); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create new pod: %w", err)
		}
		// 步骤三，等待新的pod进入Ready状态
		if !r.waitPodReady(newPod) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		time.Sleep(3 * time.Second) // 制造一定的延迟，确保已经开始双冗的阶段，保证流量数据不会丢失
		// 步骤四，删除旧的pod
		if err := r.Delete(ctx, &pod); err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 所有pod都已经升级完成，更新集群状态
	cluster.Status.CurrentVersion = cluster.Spec.Image
	if err := r.Update(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update cluster status: %w", err)
	}
	return ctrl.Result{}, nil
}

// 创建一个新的pod
func (r *HubbleClusterReconciler) buildPod(cluster *hubblev1.HubbleCluster, uuid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", cluster.Name, uuid[:8]),
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"hubble-cluster": cluster.Name,
				"hubble-uuid":    uuid,
				"hubble-version": cluster.Spec.Image,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:    "hubblecatch",
				Image:   cluster.Spec.Image,
				Env:     cluster.Spec.Env,
				EnvFrom: cluster.Spec.EnvFrom,
				Ports:   []corev1.ContainerPort{{ContainerPort: 8080}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/ready",
							Port: intstr.FromInt(8080),
						},
					},
					InitialDelaySeconds: 5,
					PeriodSeconds:       5,
					FailureThreshold:    5,
				},
				LivenessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/healthz",
							Port: intstr.FromInt(8080),
						},
					},
					InitialDelaySeconds: 15,
					PeriodSeconds:       5,
				},
				StartupProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/startup",
							Port: intstr.FromInt(8080),
						},
					},
					FailureThreshold: 10,
					PeriodSeconds:    6,
				},
			},
			},
		},
	}
}

// 发送停止接收MQ消息的信号
func (r *HubbleClusterReconciler) sendStopSignal(pod *corev1.Pod) (bool, error) {
	resp, err := http.Post(
		fmt.Sprintf("http://%s:8080/stop", pod.Status.PodIP),
		"",
		nil,
	)
	if err != nil {
		return false, nil // 网络问题允许重试
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

// 等待 Pod 进入 Ready 状态
func (r *HubbleClusterReconciler) waitPodReady(pod *corev1.Pod) bool {
	for i := 0; i < 45; i++ {
		key := client.ObjectKeyFromObject(pod)
		if err := r.Get(context.Background(), key, pod); err == nil {
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return true
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *HubbleClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hubblev1.HubbleCluster{}).
		Complete(r)
}
