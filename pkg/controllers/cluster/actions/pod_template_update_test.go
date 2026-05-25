// Copyright (C) 2026 ScyllaDB

package actions

import (
	"context"
	"testing"

	"github.com/scylladb/go-log"
	scyllav1 "github.com/scylladb/scylla-operator/pkg/api/v1"
	clusterresource "github.com/scylladb/scylla-operator/pkg/controllers/cluster/resource"
	"github.com/scylladb/scylla-operator/pkg/test/unit"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// executeAndGet runs the action against a fake client seeded with `sts`, then
// returns the post-update STS so tests can assert on it.
func executeAndGet(t *testing.T, cluster *scyllav1.ScyllaCluster, sts *appsv1.StatefulSet) *appsv1.StatefulSet {
	t.Helper()
	ctx := context.Background()
	logger, _ := log.NewProduction(log.Config{
		Level: zap.NewAtomicLevelAt(zapcore.DebugLevel),
	})
	kubeClient := fake.NewSimpleClientset([]runtime.Object{sts}...)
	a := NewPodTemplateUpdate(cluster, "image", logger)
	if err := a.Execute(ctx, &State{kubeclient: kubeClient}); err != nil {
		t.Fatal(err)
	}
	got, err := kubeClient.AppsV1().StatefulSets(cluster.Namespace).Get(ctx, sts.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get sts: %s", err)
	}
	return got
}

func TestPodTemplateUpdate_ServiceAccountAndLegacyAlias(t *testing.T) {
	const newSA = "custom-sa"

	cluster := unit.NewMultiRackCluster(1)
	cluster.Spec.ServiceAccountName = newSA

	rackSts := clusterresource.StatefulSetForRack(cluster.Spec.Datacenter.Racks[0], cluster, "image")
	// Wind back to the historical default so the action has something to reconcile,
	// and pre-populate the legacy alias as the apiserver would after defaulting.
	rackSts.Spec.Template.Spec.ServiceAccountName = "test-cluster-member"
	rackSts.Spec.Template.Spec.DeprecatedServiceAccount = "test-cluster-member"

	got := executeAndGet(t, cluster, rackSts)
	if got.Spec.Template.Spec.ServiceAccountName != newSA {
		t.Fatalf("ServiceAccountName=%q, want %q", got.Spec.Template.Spec.ServiceAccountName, newSA)
	}
	if got.Spec.Template.Spec.DeprecatedServiceAccount != newSA {
		t.Fatalf("DeprecatedServiceAccount=%q, want %q (legacy alias must stay in sync)",
			got.Spec.Template.Spec.DeprecatedServiceAccount, newSA)
	}
}

func TestPodTemplateUpdate_TolerationsAffinityLabels(t *testing.T) {
	cluster := unit.NewMultiRackCluster(1)
	rack := &cluster.Spec.Datacenter.Racks[0]

	rack.Placement = &scyllav1.PlacementSpec{
		Tolerations: []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "scylla", Effect: corev1.TaintEffectNoSchedule}},
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key: "role", Operator: corev1.NodeSelectorOpIn, Values: []string{"db"},
					}},
				}},
			},
		},
	}
	rack.CustomLabels = map[string]string{"team": "data"}

	// Build an STS as it existed *before* the user added placement/customLabels —
	// without those fields the actual rack sts diverges from desired.
	staleCluster := unit.NewMultiRackCluster(1)
	staleSts := clusterresource.StatefulSetForRack(staleCluster.Spec.Datacenter.Racks[0], staleCluster, "image")

	got := executeAndGet(t, cluster, staleSts)

	if !apiequality.Semantic.DeepEqual(got.Spec.Template.Spec.Tolerations, rack.Placement.Tolerations) {
		t.Fatalf("Tolerations not propagated: got %+v, want %+v",
			got.Spec.Template.Spec.Tolerations, rack.Placement.Tolerations)
	}
	if !apiequality.Semantic.DeepEqual(got.Spec.Template.Spec.Affinity.NodeAffinity, rack.Placement.NodeAffinity) {
		t.Fatalf("NodeAffinity not propagated: got %+v, want %+v",
			got.Spec.Template.Spec.Affinity.NodeAffinity, rack.Placement.NodeAffinity)
	}
	if v, ok := got.Spec.Template.ObjectMeta.Labels["team"]; !ok || v != "data" {
		t.Fatalf("CustomLabels not propagated to pod template labels: got %v", got.Spec.Template.ObjectMeta.Labels)
	}
}

func TestPodTemplateUpdate_ContainerResources(t *testing.T) {
	cluster := unit.NewMultiRackCluster(1)
	rack := &cluster.Spec.Datacenter.Racks[0]
	rack.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		},
	}
	rack.AgentResources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}

	staleCluster := unit.NewMultiRackCluster(1)
	staleSts := clusterresource.StatefulSetForRack(staleCluster.Spec.Datacenter.Racks[0], staleCluster, "image")

	got := executeAndGet(t, cluster, staleSts)

	scylla := findContainer(t, got.Spec.Template.Spec.Containers, "scylla")
	if !apiequality.Semantic.DeepEqual(scylla.Resources, rack.Resources) {
		t.Fatalf("Scylla container Resources not propagated: got %+v, want %+v", scylla.Resources, rack.Resources)
	}
	agent := findContainer(t, got.Spec.Template.Spec.Containers, "scylla-manager-agent")
	if !apiequality.Semantic.DeepEqual(agent.Resources, rack.AgentResources) {
		t.Fatalf("Agent container Resources not propagated: got %+v, want %+v", agent.Resources, rack.AgentResources)
	}
}

func TestPodTemplateUpdate_ForcesRollingUpdate(t *testing.T) {
	// Simulate the operator crashing mid version-upgrade with the STS left on
	// OnDelete: a template mutation alone would never roll pods, and the
	// rackSynchronized readiness check would never converge.
	cluster := unit.NewMultiRackCluster(1)
	cluster.Spec.ServiceAccountName = "custom-sa"

	rackSts := clusterresource.StatefulSetForRack(cluster.Spec.Datacenter.Racks[0], cluster, "image")
	rackSts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type: appsv1.OnDeleteStatefulSetStrategyType,
	}

	got := executeAndGet(t, cluster, rackSts)
	if got.Spec.UpdateStrategy.Type != appsv1.RollingUpdateStatefulSetStrategyType {
		t.Fatalf("UpdateStrategy=%q, want %q", got.Spec.UpdateStrategy.Type, appsv1.RollingUpdateStatefulSetStrategyType)
	}
}

func TestPodTemplateUpdate_NoopWhenInSync(t *testing.T) {
	// When desired and actual already match, RackUpdated must be true so that
	// nextAction does not pick this path on every reconcile loop.
	cluster := unit.NewMultiRackCluster(1)
	rack := cluster.Spec.Datacenter.Racks[0]
	rackSts := clusterresource.StatefulSetForRack(rack, cluster, "image")

	sub := NewPodTemplateUpdateSubAction(cluster, "image")
	updated, err := sub.RackUpdated(rack, rackSts)
	if err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatalf("RackUpdated reported divergence for an STS freshly built from the same spec")
	}
}

func findContainer(t *testing.T, cs []corev1.Container, name string) corev1.Container {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("container %q not found among %d containers", name, len(cs))
	return corev1.Container{}
}
