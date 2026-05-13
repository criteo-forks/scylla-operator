package naming

import (
	"testing"

	scyllav1 "github.com/scylladb/scylla-operator/pkg/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServiceAccountNameForMembers(t *testing.T) {
	tests := []struct {
		name    string
		cluster *scyllav1.ScyllaCluster
		want    string
	}{
		{
			name: "defaults to <name>-member when ServiceAccountName is unset (backward compat for existing clusters)",
			cluster: &scyllav1.ScyllaCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "my-cluster"},
				Spec:       scyllav1.ClusterSpec{},
			},
			want: "my-cluster-member",
		},
		{
			name: "returns the custom ServiceAccountName when set",
			cluster: &scyllav1.ScyllaCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "my-cluster"},
				Spec:       scyllav1.ClusterSpec{ServiceAccountName: "custom-sa"},
			},
			want: "custom-sa",
		},
		{
			name: "empty string is treated as unset and falls back to default",
			cluster: &scyllav1.ScyllaCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "my-cluster"},
				Spec:       scyllav1.ClusterSpec{ServiceAccountName: ""},
			},
			want: "my-cluster-member",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ServiceAccountNameForMembers(tt.cluster)
			if got != tt.want {
				t.Fatalf("ServiceAccountNameForMembers() = %q, want %q", got, tt.want)
			}
		})
	}
}
