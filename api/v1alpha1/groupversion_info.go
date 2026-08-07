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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "cells.kubeswift.io", Version: "v1alpha1"}

	// SchemeBuilder registers the Go types with a scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
