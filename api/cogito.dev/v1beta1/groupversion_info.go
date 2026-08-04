// +groupName=llm.cogito.dev
// +versionName=v1beta1
package cogitodevv1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const GroupName = "llm.cogito.dev"

var GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1beta1"}

func Kind(kind string) schema.GroupKind {
	return GroupVersion.WithKind(kind).GroupKind()
}

var SchemeBuilder = &schemeBuilder{}

type schemeBuilder struct{}

func (s *schemeBuilder) Register(types ...runtime.Object) {
	schemeTypes = append(schemeTypes, types...)
}

var schemeTypes []runtime.Object

func AddToScheme(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, schemeTypes...)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
