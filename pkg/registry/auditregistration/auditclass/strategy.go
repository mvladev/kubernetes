/*
Copyright 2019 The Kubernetes Authors.

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

package auditclass

import (
	"context"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	audit "k8s.io/kubernetes/pkg/apis/auditregistration"
	"k8s.io/kubernetes/pkg/apis/auditregistration/validation"
)

// auditClassStrategy implements verification logic for AuditClass.
type auditClassStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

// Strategy is the default logic that applies when creating and updating AuditClass objects.
var Strategy = auditClassStrategy{legacyscheme.Scheme, names.SimpleNameGenerator}

// NamespaceScoped returns false because all AuditClasses need to be cluster scoped.
func (auditClassStrategy) NamespaceScoped() bool {
	return false
}

// PrepareForCreate clears the status of an AuditClass before creation.
func (auditClassStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	ic := obj.(*audit.AuditClass)
	ic.Generation = 1
}

// PrepareForUpdate clears fields that are not allowed to be set by end users on update.
func (auditClassStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newIC := obj.(*audit.AuditClass)
	oldIC := old.(*audit.AuditClass)

	// Any changes to the class or backend increment the generation number
	// See metav1.ObjectMeta description for more information on Generation.
	if !apiequality.Semantic.DeepEqual(oldIC.Spec, newIC.Spec) {
		newIC.Generation = oldIC.Generation + 1
	}
}

// Validate validates a new auditClass.
func (auditClassStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	ic := obj.(*audit.AuditClass)
	return validation.ValidateAuditClass(ic)
}

// Canonicalize normalizes the object after validation.
func (auditClassStrategy) Canonicalize(obj runtime.Object) {
}

// AllowCreateOnUpdate is true for auditClass; this means you may create one with a PUT request.
func (auditClassStrategy) AllowCreateOnUpdate() bool {
	return false
}

// ValidateUpdate is the default update validation for an end user.
func (auditClassStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	validationErrorList := validation.ValidateAuditClass(obj.(*audit.AuditClass))
	return append(validationErrorList)
}

// AllowUnconditionalUpdate is the default update policy for auditClass objects. Status update should
// only be allowed if version match.
func (auditClassStrategy) AllowUnconditionalUpdate() bool {
	return false
}
