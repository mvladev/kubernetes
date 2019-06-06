/*
Copyright 2018 The Kubernetes Authors.

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

package policy

import (
	"k8s.io/api/auditregistration/v1alpha1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/authorization/authorizer"
)

// ConvertDynamicPolicyToInternal constructs an internal policy type from a
// v1alpha1 dynamic Policy and AuditClass RequestSelectors map indexed by class name.
func ConvertDynamicPolicyToInternal(p *v1alpha1.Policy, auditClassSelectors map[string][]v1alpha1.RequestSelector) *audit.Policy {
	policy := &audit.Policy{}
	policyStagesStrings := convertDynamicStagesToStrings(p.Stages)
	policyStagesStringSet := sets.NewString(policyStagesStrings...)
	rules := make([]audit.PolicyRule, len(p.Rules))
	for ri, rule := range p.Rules {
		if requestSelectors, ok := auditClassSelectors[rule.WithAuditClass]; ok {
			var ruleStages []audit.Stage
			if len(rule.Stages) > 0 {
				ruleStagesStrings := convertDynamicStagesToStrings(rule.Stages)
				ruleStagesStringSet := sets.NewString(ruleStagesStrings...)
				policyStagesStringSet = policyStagesStringSet.Union(ruleStagesStringSet)
				ruleStages = ConvertStringSetToStages(ruleStagesStringSet)
				ruleStages = InvertStages(ruleStages)
			}
			ruleLevel := audit.Level(rule.Level)
			if ruleLevel == "" {
				ruleLevel = audit.Level(p.Level)
			}
			for _, rs := range requestSelectors {
				r := audit.PolicyRule{
					Level: ruleLevel,
				}
				// all properties but Level are optional
				if len(rs.Users) > 0 {
					r.Users = rs.Users
				}
				if len(rs.UserGroups) > 0 {
					r.UserGroups = rs.UserGroups
				}
				if len(rs.Verbs) > 0 {
					r.Verbs = rs.Verbs
				}
				if len(rs.Namespaces) > 0 {
					r.Namespaces = rs.Namespaces
				}
				if len(rs.NonResourceURLs) > 0 {
					r.NonResourceURLs = rs.NonResourceURLs
				}
				if len(ruleStages) > 0 {
					r.OmitStages = ruleStages
				}
				resources := make([]audit.GroupResources, len(rs.Resources))
				for gri, gr := range rs.Resources {
					resources[gri] = audit.GroupResources{
						Group:         gr.Group,
						Resources:     gr.Resources,
						ResourceNames: gr.ObjectNames,
					}
				}
				if len(resources) > 0 {
					r.Resources = resources
				}
				rules[ri] = r
			}
		}
	}
	policyStages := ConvertStringSetToStages(policyStagesStringSet)
	policyStages = InvertStages(policyStages)
	if len(policyStages) > 0 {
		policy.OmitStages = policyStages
	}
	if len(rules) > 0 {
		policy.Rules = rules
	} else if p.Level != "" {
		policy.Rules = []audit.PolicyRule{
			{
				Level: audit.Level(p.Level),
			},
		}
	}
	return policy
}

// convertDynamicStagesToStrings converts an array of stages to a string array
func convertDynamicStagesToStrings(stages []v1alpha1.Stage) []string {
	s := make([]string, len(stages))
	for i, stage := range stages {
		s[i] = string(stage)
	}
	return s
}

// convertDynamicStringSetToStages converts a string set to an array of stages
func convertDynamicStringSetToStages(set sets.String) []v1alpha1.Stage {
	stages := make([]v1alpha1.Stage, len(set))
	for i, stage := range set.List() {
		stages[i] = v1alpha1.Stage(stage)
	}
	return stages
}

// NewDynamicChecker returns a new dynamic policy checker
func NewDynamicChecker() Checker {
	return &dynamicPolicyChecker{}
}

type dynamicPolicyChecker struct{}

// LevelAndStages returns returns a fixed level of the full event, this is so that the downstream policy
// can be applied per sink.
// TODO: this needs benchmarking before the API moves to beta to determine the effect this has on the apiserver
func (d *dynamicPolicyChecker) LevelAndStages(authorizer.Attributes) (audit.Level, []audit.Stage) {
	return audit.LevelRequestResponse, []audit.Stage{}
}
