// +groupName=llm.cogito.dev
// +versionName=v1alpha1
package cogitodevv1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is the group name used in API definitions.
const GroupName = "llm.cogito.dev"

// GroupVersion is group version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

// Kind takes an unqualified kind and returns back a Group qualified GroupKind.
func Kind(kind string) schema.GroupKind {
	return GroupVersion.WithKind(kind).GroupKind()
}

// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
var SchemeBuilder = &schemeBuilder{}

type schemeBuilder struct{}

// Register adds types to the internal registry for AddToScheme.
func (s *schemeBuilder) Register(types ...runtime.Object) {
	schemeTypes = append(schemeTypes, types...)
}

var schemeTypes []runtime.Object

// AddToScheme adds the types in this group-version to the given scheme.
func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, schemeTypes...)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
