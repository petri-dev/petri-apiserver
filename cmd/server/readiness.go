package main

import (
	"context"
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	petriGroup           = "core.petri.run"
	resourceEnvironments = "ephemeralenvironments"
	resourceTemplates    = "environmenttemplates"
)

func dependencyCheck(cl dynamic.Interface, namespace string) func(context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
		defer cancel()

		ns, err := cl.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).
			Get(ctx, namespace, metav1.GetOptions{})
		if err != nil || ns.GetDeletionTimestamp() != nil {
			return errors.New("management namespace unavailable")
		}

		for _, resource := range []string{resourceEnvironments, resourceTemplates} {
			_, listErr := cl.Resource(schema.GroupVersionResource{Group: petriGroup, Version: "v1alpha1", Resource: resource}).
				Namespace(namespace).
				List(ctx, metav1.ListOptions{Limit: 1})
			if listErr != nil {
				return errors.New("required resource unavailable")
			}
		}

		return nil
	}
}
