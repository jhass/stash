/*
Copyright AppsCode Inc. and Contributors

Licensed under the AppsCode Community License 1.0.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://github.com/appscode/licenses/raw/1.0.0/AppsCode-Community-1.0.0.md

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rbac

import (
	"context"
	"fmt"
	"strings"

	"stash.appscode.dev/apimachinery/apis"
	api "stash.appscode.dev/apimachinery/apis/stash/v1alpha1"
	api_v1beta1 "stash.appscode.dev/apimachinery/apis/stash/v1beta1"

	coordination "k8s.io/api/coordination/v1"
	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	core_util "kmodules.xyz/client-go/core/v1"
	meta_util "kmodules.xyz/client-go/meta"
	rbac_util "kmodules.xyz/client-go/rbac/v1"
	wapi "kmodules.xyz/webhook-runtime/apis/workload/v1"
)

func (opt *Options) EnsureRestoreInitContainerRBAC() error {
	// ensure ClusterRole for restore init container
	err := opt.ensureRestoreInitContainerClusterRole()
	if err != nil {
		return err
	}

	// ensure RoleBinding for restore init container
	err = opt.ensureRestoreInitContainerRoleBinding()
	if err != nil {
		return err
	}

	return opt.ensureCrossNamespaceRBAC()
}

func (opt *Options) ensureRestoreInitContainerClusterRole() error {
	meta := metav1.ObjectMeta{
		Name:   apis.StashRestoreInitContainerClusterRole,
		Labels: opt.offshootLabels,
	}
	_, _, err := rbac_util.CreateOrPatchClusterRole(context.TODO(), opt.kubeClient, meta, func(in *rbac.ClusterRole) *rbac.ClusterRole {
		in.Rules = []rbac.PolicyRule{
			{
				APIGroups: []string{api_v1beta1.SchemeGroupVersion.Group},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
			{
				APIGroups: []string{api.SchemeGroupVersion.Group},
				Resources: []string{"*"},
				Verbs:     []string{"*"},
			},
			{
				APIGroups: []string{core.GroupName},
				Resources: []string{"configmaps"},
				Verbs:     []string{"create", "update", "get"},
			},
			{
				APIGroups: []string{core.GroupName},
				Resources: []string{"pods/exec"},
				Verbs:     []string{"get", "create"},
			},
			{
				APIGroups: []string{core.GroupName},
				Resources: []string{"events"},
				Verbs:     []string{"create"},
			},
			{
				APIGroups: []string{core.SchemeGroupVersion.Group},
				Resources: []string{"secrets", "endpoints", "pods"},
				Verbs:     []string{"get"},
			},
			{
				APIGroups: []string{coordination.GroupName},
				Resources: []string{"leases"},
				Verbs:     []string{"*"},
			},
		}
		return in
	}, metav1.PatchOptions{})
	return err
}

func (opt *Options) ensureRestoreInitContainerRoleBinding() error {
	meta := metav1.ObjectMeta{
		Namespace: opt.invOpts.Namespace,
		Name:      getRestoreInitContainerRoleBindingName(opt.owner.Kind, opt.owner.Name),
		Labels:    opt.offshootLabels,
	}
	_, _, err := rbac_util.CreateOrPatchRoleBinding(context.TODO(), opt.kubeClient, meta, func(in *rbac.RoleBinding) *rbac.RoleBinding {
		core_util.EnsureOwnerReference(&in.ObjectMeta, opt.owner)

		if in.Annotations == nil {
			in.Annotations = map[string]string{}
		}

		in.RoleRef = rbac.RoleRef{
			APIGroup: rbac.GroupName,
			Kind:     apis.KindClusterRole,
			Name:     apis.StashRestoreInitContainerClusterRole,
		}
		in.Subjects = []rbac.Subject{
			{
				Kind:      rbac.ServiceAccountKind,
				Name:      opt.serviceAccount.Name,
				Namespace: opt.serviceAccount.Namespace,
			},
		}
		return in
	}, metav1.PatchOptions{})
	return err
}

func getRestoreInitContainerRoleBindingName(kind, name string) string {
	return meta_util.ValidNameWithPrefixNSuffix(apis.StashRestoreInitContainerClusterRole, strings.ToLower(kind), name)
}

func ensureRestoreInitContainerRoleBindingDeleted(kubeClient kubernetes.Interface, logger klog.Logger, w *wapi.Workload) error {
	err := kubeClient.RbacV1().RoleBindings(w.Namespace).Delete(
		context.TODO(),
		getRestoreInitContainerRoleBindingName(w.Kind, w.Name),
		metav1.DeleteOptions{})
	if err != nil {
		if kerr.IsNotFound(err) {
			return nil
		}
		return err
	}
	logger.Info(fmt.Sprintf("RoleBinding %s/%s has been deleted",
		w.Namespace,
		getRestoreInitContainerRoleBindingName(w.Kind, w.Name),
	))
	return nil
}
