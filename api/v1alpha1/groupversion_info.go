// Package v1alpha1 contains the cells.kubeswift.io/v1alpha1 API.
//
// GPUCellPool is a composition layer: it creates KubeSwift SwiftGuest VMs that
// hold a whole passthrough GPU and join a workload Kubernetes cluster, where HAMi
// shares that GPU between workloads. KubeSwift owns physical GPU -> VM; HAMi owns
// GPU -> workload. This API never translates between the two.
//
// +kubebuilder:object:generate=true
// +groupName=cells.kubeswift.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "cells.kubeswift.io", Version: "v1alpha1"}

	// SchemeBuilder registers the Go types with a scheme.
	//
	// apimachinery's builder rather than controller-runtime's scheme.Builder,
	// which controller-runtime deprecated: an API package is imported by anyone
	// who wants the types, so it should not drag a controller framework in with
	// it. This package now depends on apimachinery alone.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes is exactly what controller-runtime's Builder.Register did, so
// registration is unchanged.
func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &GPUCellPool{}, &GPUCellPoolList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
