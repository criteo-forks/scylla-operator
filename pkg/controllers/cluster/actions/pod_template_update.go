// Copyright (C) 2026 ScyllaDB

package actions

import (
	"github.com/pkg/errors"
	"github.com/scylladb/go-log"
	scyllav1 "github.com/scylladb/scylla-operator/pkg/api/v1"
	"github.com/scylladb/scylla-operator/pkg/controllers/cluster/resource"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
)

const (
	PodTemplateUpdateAction = "pod-template-update"
)

// PodTemplateUpdate propagates spec-derived pod-template settings from the
// cluster spec onto rack StatefulSets when they diverge. It reconciles a
// curated subset of fields that govern scheduling, identity, labelling, and
// container resources:
//
//   - ServiceAccountName (plus the legacy "serviceAccount" alias)
//   - Affinity (node/pod/anti-pod)
//   - Tolerations
//   - Pod template labels (RackLabels + spec.CustomLabels)
//   - Per-container Resources, matched by container name
//
// Image-related fields are intentionally out of scope: SidecarUpgrade and the
// version-upgrade action own those and run earlier in nextAction. Volumes and
// network mode are also out of scope; they involve more invasive changes that
// deserve dedicated handling.
type PodTemplateUpdate struct {
	cluster      *scyllav1.ScyllaCluster
	sidecarImage string
}

func (a *PodTemplateUpdate) RackUpdated(rack scyllav1.RackSpec, sts *appsv1.StatefulSet) (bool, error) {
	desired := resource.StatefulSetForRack(rack, a.cluster, a.sidecarImage)
	return !managedPodTemplateDiverged(sts, desired), nil
}

func (a *PodTemplateUpdate) Update(rack scyllav1.RackSpec, sts *appsv1.StatefulSet) error {
	desired := resource.StatefulSetForRack(rack, a.cluster, a.sidecarImage)
	wantSpec := desired.Spec.Template.Spec
	actualSpec := &sts.Spec.Template.Spec

	actualSpec.ServiceAccountName = wantSpec.ServiceAccountName
	// Keep DeprecatedServiceAccount (JSON: "serviceAccount") in sync. The apiserver
	// rejects updates when both fields are set to different values, and read-back of
	// an existing pod template typically populates this legacy alias via defaulting.
	actualSpec.DeprecatedServiceAccount = wantSpec.ServiceAccountName

	actualSpec.Affinity = wantSpec.Affinity
	actualSpec.Tolerations = wantSpec.Tolerations

	sts.Spec.Template.ObjectMeta.Labels = desired.Spec.Template.ObjectMeta.Labels

	if err := applyContainerResources(actualSpec.Containers, wantSpec.Containers); err != nil {
		return errors.Wrap(err, "apply container resources")
	}

	// Force RollingUpdate so the mutation actually rolls pods. The STS may have
	// been left in OnDelete by a previously crashed version-upgrade action; the
	// rackSynchronized readiness check (UpdateRevision==CurrentRevision) cannot
	// converge under OnDelete from a pod-template mutation alone.
	sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{
		Type: appsv1.RollingUpdateStatefulSetStrategyType,
	}
	return nil
}

func NewPodTemplateUpdate(c *scyllav1.ScyllaCluster, sidecarImage string, l log.Logger) *rackSynchronizedAction {
	return &rackSynchronizedAction{
		subAction: NewPodTemplateUpdateSubAction(c, sidecarImage),
		cluster:   c,
		logger:    l,
	}
}

// NewPodTemplateUpdateSubAction exposes the rack-level diff/apply primitive so
// callers (e.g. nextAction) can probe whether any rack diverges without first
// constructing the full rack-synchronized wrapper.
func NewPodTemplateUpdateSubAction(c *scyllav1.ScyllaCluster, sidecarImage string) *PodTemplateUpdate {
	return &PodTemplateUpdate{
		cluster:      c,
		sidecarImage: sidecarImage,
	}
}

func (a *PodTemplateUpdate) Name() string {
	return PodTemplateUpdateAction
}

// managedPodTemplateDiverged returns true when any field this action owns
// diverges between actual and desired. Keep this set aligned with Update.
func managedPodTemplateDiverged(actual, desired *appsv1.StatefulSet) bool {
	actualSpec := actual.Spec.Template.Spec
	wantSpec := desired.Spec.Template.Spec

	if actualSpec.ServiceAccountName != wantSpec.ServiceAccountName {
		return true
	}
	// The apiserver populates DeprecatedServiceAccount on read via defaulting,
	// so once set it must agree with ServiceAccountName or k8s validation will
	// reject the next update.
	if actualSpec.DeprecatedServiceAccount != "" && actualSpec.DeprecatedServiceAccount != wantSpec.ServiceAccountName {
		return true
	}
	if !apiequality.Semantic.DeepEqual(actualSpec.Affinity, wantSpec.Affinity) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(actualSpec.Tolerations, wantSpec.Tolerations) {
		return true
	}
	if !apiequality.Semantic.DeepEqual(actual.Spec.Template.ObjectMeta.Labels, desired.Spec.Template.ObjectMeta.Labels) {
		return true
	}
	if containerResourcesDiverge(actualSpec.Containers, wantSpec.Containers) {
		return true
	}
	// If a previous upgrade left the STS in OnDelete, a template mutation would
	// not roll pods on its own. Treat that as a divergence so we converge back
	// to RollingUpdate together with any field change.
	if actual.Spec.UpdateStrategy.Type != appsv1.RollingUpdateStatefulSetStrategyType {
		return true
	}
	return false
}

// containerResourcesDiverge reports whether any container present in both
// actual and want (matched by name) has different Resources. Containers that
// exist only on one side are ignored: container set management belongs to
// StatefulSetForRack / dedicated actions, not this one.
func containerResourcesDiverge(actual, want []corev1.Container) bool {
	byName := make(map[string]corev1.ResourceRequirements, len(want))
	for _, c := range want {
		byName[c.Name] = c.Resources
	}
	for _, c := range actual {
		wantRes, ok := byName[c.Name]
		if !ok {
			continue
		}
		if !apiequality.Semantic.DeepEqual(c.Resources, wantRes) {
			return true
		}
	}
	return false
}

// applyContainerResources copies Resources from want into matching containers
// in actual (matched by name). Containers in actual that have no counterpart
// in want are left untouched.
func applyContainerResources(actual, want []corev1.Container) error {
	byName := make(map[string]corev1.ResourceRequirements, len(want))
	for _, c := range want {
		byName[c.Name] = c.Resources
	}
	for i := range actual {
		if r, ok := byName[actual[i].Name]; ok {
			actual[i].Resources = r
		}
	}
	return nil
}
