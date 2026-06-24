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
//   - Per-container VolumeMounts, matched by container name
//   - Pod Volumes (operator-managed base volumes + spec.Volumes)
//
// Image-related fields are intentionally out of scope: SidecarUpgrade and the
// version-upgrade action own those and run earlier in nextAction. Network mode
// is also out of scope; it involves more invasive changes that deserve
// dedicated handling.
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

	if err := applyContainerSpec(actualSpec.Containers, wantSpec.Containers); err != nil {
		return errors.Wrap(err, "apply container spec")
	}

	// Replace the full Volumes list with the desired one. The apiserver does not
	// inject volumes into the STS pod template (the ServiceAccount token volume is
	// added to Pods, not the template), so a full replacement is safe and also
	// supports removing volumes the user dropped from the spec. defaultMode that
	// the apiserver fills in on read-back is reconciled away by volumesDiverge.
	actualSpec.Volumes = deepCopyVolumes(wantSpec.Volumes)

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
	if containerSpecDiverge(actualSpec.Containers, wantSpec.Containers) {
		return true
	}
	if volumesDiverge(actualSpec.Volumes, wantSpec.Volumes) {
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

// containerSpecDiverge reports whether any container present in both actual and
// want (matched by name) has different Resources or VolumeMounts. Containers
// that exist only on one side are ignored: container set management belongs to
// StatefulSetForRack / dedicated actions, not this one. VolumeMounts are not
// server-defaulted by the apiserver, so a direct comparison is loop-safe.
func containerSpecDiverge(actual, want []corev1.Container) bool {
	byName := make(map[string]*corev1.Container, len(want))
	for i := range want {
		byName[want[i].Name] = &want[i]
	}
	for i := range actual {
		w, ok := byName[actual[i].Name]
		if !ok {
			continue
		}
		if !apiequality.Semantic.DeepEqual(actual[i].Resources, w.Resources) {
			return true
		}
		if !apiequality.Semantic.DeepEqual(actual[i].VolumeMounts, w.VolumeMounts) {
			return true
		}
	}
	return false
}

// volumesDiverge reports whether the actual and desired Volumes lists differ,
// ignoring the volume-source defaultMode that the apiserver fills in via
// defaulting on read-back (ConfigMap/Secret/Projected/DownwardAPI sources
// default to 0644). Comparing those raw would flag a permanent divergence and
// spin the reconcile loop forever, since the desired list built from the spec
// leaves defaultMode unset. Order matters: StatefulSetForRack emits volumes in
// a deterministic order and Update writes that same order back, so a positional
// comparison is sufficient.
func volumesDiverge(actual, want []corev1.Volume) bool {
	return !apiequality.Semantic.DeepEqual(normalizeVolumes(actual), normalizeVolumes(want))
}

// deepCopyVolumes returns an independent copy of the volumes slice so mutating
// the STS does not alias the freshly-built desired template.
func deepCopyVolumes(in []corev1.Volume) []corev1.Volume {
	if in == nil {
		return nil
	}
	out := make([]corev1.Volume, len(in))
	for i := range in {
		out[i] = *in[i].DeepCopy()
	}
	return out
}

// normalizeVolumes returns a copy of the volumes with apiserver-defaulted
// defaultMode fields cleared, so equality checks compare only spec-derived
// fields. The originals are left untouched.
func normalizeVolumes(in []corev1.Volume) []corev1.Volume {
	out := deepCopyVolumes(in)
	for i := range out {
		src := &out[i].VolumeSource
		if src.ConfigMap != nil {
			src.ConfigMap.DefaultMode = nil
		}
		if src.Secret != nil {
			src.Secret.DefaultMode = nil
		}
		if src.Projected != nil {
			src.Projected.DefaultMode = nil
		}
		if src.DownwardAPI != nil {
			src.DownwardAPI.DefaultMode = nil
		}
	}
	return out
}

// applyContainerSpec copies Resources and VolumeMounts from want into matching
// containers in actual (matched by name). Containers in actual that have no
// counterpart in want are left untouched. The apiserver does not inject
// VolumeMounts into the STS pod template (the ServiceAccount token mount is
// added to Pods, not the template), so replacing the whole list is safe and
// supports removing mounts the user dropped from the spec.
func applyContainerSpec(actual, want []corev1.Container) error {
	byName := make(map[string]*corev1.Container, len(want))
	for i := range want {
		byName[want[i].Name] = &want[i]
	}
	for i := range actual {
		w, ok := byName[actual[i].Name]
		if !ok {
			continue
		}
		actual[i].Resources = w.Resources
		actual[i].VolumeMounts = deepCopyVolumeMounts(w.VolumeMounts)
	}
	return nil
}

// deepCopyVolumeMounts returns an independent copy of the volume mounts slice so
// mutating the STS does not alias the freshly-built desired template.
func deepCopyVolumeMounts(in []corev1.VolumeMount) []corev1.VolumeMount {
	if in == nil {
		return nil
	}
	out := make([]corev1.VolumeMount, len(in))
	copy(out, in)
	return out
}
