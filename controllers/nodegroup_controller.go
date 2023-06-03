/*
Copyright 2023.

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

package controllers

import (
	"context"
	"fmt"

	certv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	meshv1 "github.com/webmeshproj/operator/api/v1"
	"github.com/webmeshproj/operator/controllers/resources"
)

// NodeGroupReconciler reconciles a NodeGroup object
type NodeGroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

const foregroundDeletion = "nodegroups.mesh.webmesh.io"

//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims;services;configmaps;pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=mesh.webmesh.io,resources=nodegroups,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=mesh.webmesh.io,resources=nodegroups/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=mesh.webmesh.io,resources=nodegroups/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *NodeGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var group meshv1.NodeGroup
	if err := r.Get(ctx, req.NamespacedName, &group); err != nil {
		if client.IgnoreNotFound(err) != nil {
			log.Error(err, "unable to fetch NodeGroup")
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if group.GetDeletionTimestamp() != nil {
		return ctrl.Result{}, r.reconcileDelete(ctx, &group)
	}

	log.Info("reconciling NodeGroup")

	// Get the mesh object
	var mesh meshv1.Mesh
	if err := r.Get(ctx, client.ObjectKey{
		Name: group.Spec.Mesh.Name,
		Namespace: func() string {
			if group.Spec.Mesh.Namespace != "" {
				return group.Spec.Mesh.Namespace
			}
			return group.GetNamespace()
		}(),
	}, &mesh); err != nil {
		log.Error(err, "unable to fetch Mesh")
		return ctrl.Result{}, err
	}

	// We need certificates for the node group no matter where they are going
	var toApply []client.Object
	for i := 0; i < int(*group.Spec.Replicas); i++ {
		toApply = append(toApply, resources.NewNodeCertificate(&mesh, &group, i))
	}
	if err := resources.Apply(ctx, r.Client, toApply); err != nil {
		log.Error(err, "unable to apply certificates")
		return ctrl.Result{}, err
	}

	var res ctrl.Result
	var err error
	if group.Spec.GoogleCloud != nil {
		res, err = r.reconcileGoogleCloudNodeGroup(ctx, &mesh, &group)
	} else if group.Spec.Cluster != nil {
		res, err = r.reconcileClusterNodeGroup(ctx, &mesh, &group)
	} else {
		err = fmt.Errorf("no deployment configuration provided")
	}
	if err != nil {
		log.Error(err, "unable to reconcile NodeGroup")
		return ctrl.Result{}, err
	}

	// Set finalizers
	if !controllerutil.ContainsFinalizer(&group, foregroundDeletion) {
		log.Info("Adding finalizer to node group")
		controllerutil.AddFinalizer(&group, foregroundDeletion)
		if err = r.Update(ctx, &group); err != nil {
			err = fmt.Errorf("add finalizer to node group: %w", err)
		}
	}
	return res, err
}

func (r *NodeGroupReconciler) reconcileDelete(ctx context.Context, group *meshv1.NodeGroup) error {
	log := log.FromContext(ctx)
	var err error
	if group.Spec.GoogleCloud != nil {
		log.Info("Deleting Google Cloud NodeGroup resources")
		err = r.deleteGoogleCloudNodeGroup(ctx, group)
	}
	// TODO: Add other providers here
	if err != nil {
		return err
	}
	// Remove the finalizer
	controllerutil.RemoveFinalizer(group, foregroundDeletion)
	if err := r.Update(ctx, group); err != nil {
		return fmt.Errorf("failed to remove finalizer from node group: %w", err)
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&meshv1.NodeGroup{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&certv1.Certificate{}).
		Complete(r)
}
