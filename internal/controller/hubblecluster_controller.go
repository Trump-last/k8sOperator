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
	"fmt"
	"hash/fnv"
	"net/http"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	hubblev1 "git.n.xiaomi.com/xulinfeng1/hubbleopt/api/v1"
)

// HubbleClusterReconciler reconciles a HubbleCluster object
type HubbleClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=hubble.example.org,resources=hubbleclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hubble.example.org,resources=hubbleclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hubble.example.org,resources=hubbleclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the HubbleCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.2/pkg/reconcile
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
	currentReplicas := len(cluster.Status.ActiveUUIDs)
	desiredReplicas := int(cluster.Spec.Replicas)
	if currentReplicas != desiredReplicas {
		if desiredReplicas > currentReplicas {
			// 生成新增的 UUID
			newUUIDs := generateUUID(desiredReplicas - currentReplicas)
			cluster.Status.ActiveUUIDs = append(cluster.Status.ActiveUUIDs, newUUIDs...)
		} else {
			// 删减多余的 UUID（仅保留前 desiredReplicas 个）
			cluster.Status.ActiveUUIDs = cluster.Status.ActiveUUIDs[:desiredReplicas]
		}
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

	// 再一次同步pod状态
	existingPods, err = r.findManagedPods(ctx, cluster)
	if err != nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("failed to sync pods before Upgrade: %w", err)
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
	// if err := r.List(ctx, &pods, client.InNamespace(cluster.Namespace),
	// 	client.MatchingLabels{"hubble-cluster": cluster.Name}); err != nil {
	// 	return nil, err
	// }
	if err := r.Client.List(ctx, &pods,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{"hubble-cluster": cluster.Name},
	); err != nil {
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
	fmt.Println("start syncPods")
	// 1. 构建活跃 UUID 集合
	activeUUIDs := make(map[string]struct{})
	for _, uuid := range cluster.Status.ActiveUUIDs {
		activeUUIDs[uuid] = struct{}{}
	}
	fmt.Println("ActiveUUIDs:", activeUUIDs)
	// 2. 遍历现有 Pod，区分活跃和非活跃状态处理
	existingUuids := make(map[string]struct{})
	for _, pod := range existingPods {
		uuid := pod.Labels["hubble-uuid"]
		if _, active := activeUUIDs[uuid]; active {
			// 若属于活跃列表：检查是否为失效 Pod，应用冷却时间
			if IsPodFailed(&pod) {
				fmt.Printf("in syncPods, %s failed", pod.Name)
				if time.Since(pod.CreationTimestamp.Time) > 1*time.Minute {
					if err := r.Delete(ctx, &pod); client.IgnoreNotFound(err) != nil {
						return fmt.Errorf("failed to delete failed pod %s: %v", pod.Name, err)
					}
				}
			} else {
				// 存活状态或有用的 Pod，记录 UUID
				existingUuids[uuid] = struct{}{}
			}
		} else {
			// 不属于活跃列表的 Pod：立即删除（无论是否失败）
			if err := r.Delete(ctx, &pod); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("failed to delete obsolete pod %s: %v", pod.Name, err)
			}
		}
	}
	fmt.Println("ExistingUUIDs:", existingUuids)
	// 创建缺失的uuid
	for _, uuid := range cluster.Status.ActiveUUIDs {
		if _, ok := existingUuids[uuid]; !ok {
			pod := r.buildPod(cluster, uuid)
			if err := controllerutil.SetControllerReference(cluster, pod, r.Scheme); err != nil {
				return err
			}
			if err := r.Create(ctx, pod); err != nil {
				if apierrors.IsAlreadyExists(err) {
					continue
				}
				return fmt.Errorf("failed to create pod: %w", err)
			}
			fmt.Println("In syncPods, create pod for uuid:", uuid)
		}
	}
	return nil
}

// Pod状态检查
func IsPodFailed(pod *corev1.Pod) bool {
	// 情况一：Pod Phase 已经是 Failed/Unknown（直接判定失败）
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodUnknown {
		return true
	}

	// 情况二：Pod长时间卡在 Pending，且有明确的错误原因（例如镜像拉取失败）
	if pod.Status.Phase == corev1.PodPending {
		for _, containerStatus := range pod.Status.ContainerStatuses {
			waiting := containerStatus.State.Waiting
			if waiting != nil &&
				(waiting.Reason == "ImagePullBackOff" ||
					waiting.Reason == "ErrImagePull" ||
					waiting.Reason == "CrashLoopBackOff") {
				return true
			}
		}
		// 如果Pending时间超过阈值（例如5分钟），可视为失败（根据实际需求添加此逻辑）
	}

	// 情况三：容器异常退出（Exit Code非0）
	for _, containerStatus := range pod.Status.ContainerStatuses {
		if term := containerStatus.State.Terminated; term != nil && term.ExitCode != 0 {
			return true
		}
	}
	return false
}

// 滚动升级（核心代码）
func (r *HubbleClusterReconciler) rollingUpgrade(
	ctx context.Context,
	cluster *hubblev1.HubbleCluster,
	existingPods []corev1.Pod,
) (ctrl.Result, error) {
	upnum := 0
	hasher := fnv.New32a()
	hasher.Write([]byte(cluster.Spec.Image))
	imageHash := fmt.Sprintf("%x", hasher.Sum32())
	fmt.Println("Start rolling upgrade with ", len(existingPods), "pods")
	for _, pod := range existingPods {
		if pod.Labels["hubble-version"] == imageHash || IsPodFailed(&pod) {
			continue // 版本一致或者pod有问题，不需要升级
		}
		// 步骤一，先发给old pod一个停止接收消息的信号,让它不要处理新的消息
		if ok, err := r.sendStopSignal(&pod); !ok || err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
		// 步骤二，创建一个新的pod，使用相同的uuid
		newPod := r.buildPod(cluster, pod.Labels["hubble-uuid"])
		fmt.Println("In rolling upgrade, create new pod for uuid:", pod.Labels["hubble-uuid"])
		if err := controllerutil.SetControllerReference(cluster, newPod, r.Scheme); err != nil {
			return ctrl.Result{}, err
		} // 要绑定controller
		if err := r.Create(ctx, newPod); err != nil {
			if apierrors.IsAlreadyExists(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("failed to create new pod: %w", err)
		}
		// 步骤三，等待新的pod进入Ready状态
		if !r.waitPodReady(newPod) {
			r.Delete(ctx, newPod) // 如果新的pod无法进入Ready状态，删除它，等待下一次重试
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		time.Sleep(3 * time.Second) // 制造一定的延迟，确保已经开始双冗的阶段，保证流量数据不会丢失
		// 步骤四，删除旧的pod
		if err := r.Delete(ctx, &pod); err != nil {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, err
		}
		upnum++
		// 继续下一个pod
	}
	if upnum == 0 {
		return ctrl.Result{}, nil
	}
	fmt.Println("Rolling upgrade finished")
	// 所有pod都已经升级完成，更新集群状态
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// 获取最新版本的资源
		latestCluster := &hubblev1.HubbleCluster{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(cluster), latestCluster); err != nil {
			return err
		}
		// 更新状态
		latestCluster.Status.CurrentVersion = latestCluster.Spec.Image
		return r.Status().Update(ctx, latestCluster)
	})

	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update cluster status: %w", err)
	}
	return ctrl.Result{}, nil
}

// 创建一个新的pod
func (r *HubbleClusterReconciler) buildPod(cluster *hubblev1.HubbleCluster, uuid string) *corev1.Pod {
	// 生成镜像版本的短哈希（8位）
	hasher := fnv.New32a()
	hasher.Write([]byte(cluster.Spec.Image))
	imageHash := fmt.Sprintf("%x", hasher.Sum32())
	name := fmt.Sprintf("%s-%s-%s",
		cluster.Name,
		uuid,
		imageHash,
	)
	// 确保名称不超过63字符（K8s限制）
	if len(name) > 63 {
		name = name[:63]
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"hubble-cluster": cluster.Name,
				"hubble-uuid":    uuid,
				"hubble-version": imageHash,
			},
		},
		Spec: corev1.PodSpec{
			SecurityContext: cluster.Spec.PodSecurity,
			Containers: []corev1.Container{{
				Name:  "hubblecatch",
				Image: cluster.Spec.Image,
				Env: append(cluster.Spec.Env,
					corev1.EnvVar{
						Name: "hubble-uuid", // 容器内环境变量名
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "metadata.labels['hubble-uuid']", // 从 Label 注入
							},
						},
					},
				),
				EnvFrom:   cluster.Spec.EnvFrom,
				Resources: cluster.Spec.Resources,
				Ports:     []corev1.ContainerPort{{ContainerPort: 8080}},
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
	client := &http.Client{Timeout: 5 * time.Second} // 设置超时
	url := fmt.Sprintf("http://%s:8080/stop", pod.Status.PodIP)
	resp, err := client.Post(url, "", nil)
	if err == nil && resp.StatusCode == http.StatusOK {
		return true, nil
	}
	return false, nil
}

// 等待 Pod 进入 Ready 状态
func (r *HubbleClusterReconciler) waitPodReady(pod *corev1.Pod) bool {
	for i := 0; i < 20; i++ {
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
		Owns(&corev1.Pod{}).
		Named("hubblecluster").
		Complete(r)
}
