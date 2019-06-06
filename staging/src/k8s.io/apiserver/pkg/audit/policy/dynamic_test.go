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
	"testing"

	"github.com/stretchr/testify/require"

	"k8s.io/api/auditregistration/v1alpha1"
	"k8s.io/apiserver/pkg/apis/audit"
)

func TestConvertDynamicPolicyToInternal(t *testing.T) {
	for _, test := range []struct {
		desc     string
		dynamic  *v1alpha1.Policy
		classes  map[string][]v1alpha1.RequestSelector
		internal *audit.Policy
	}{
		{
			desc: "should convert full",
			dynamic: &v1alpha1.Policy{
				Level: v1alpha1.LevelNone,
				Stages: []v1alpha1.Stage{
					v1alpha1.StageResponseComplete,
				},
				Rules: []v1alpha1.PolicyRule{
					{
						WithAuditClass: "CompleteRule",
						Level:          v1alpha1.LevelRequest,
						Stages: []v1alpha1.Stage{
							v1alpha1.StageRequestReceived,
							v1alpha1.StageResponseStarted,
						},
					},
					{
						WithAuditClass: "MinimalRule",
						Level:          v1alpha1.LevelMetadata,
					},
				},
			},
			classes: map[string][]v1alpha1.RequestSelector{
				"CompleteRule": []v1alpha1.RequestSelector{
					{
						Users:           []string{"system:kube-proxy"},
						UserGroups:      []string{"system:authenticated"},
						Verbs:           []string{"watch"},
						Namespaces:      []string{"test"},
						NonResourceURLs: []string{"/status"},
						Resources: []v1alpha1.GroupResources{
							{
								Group:       "",
								Resources:   []string{"configmaps", "secrets"},
								ObjectNames: []string{"controller-leader"},
							},
						},
					},
				},
				"MinimalRule": []v1alpha1.RequestSelector{
					{
						UserGroups: []string{"system:authenticated"},
						Verbs:      []string{"patch"},
						Resources: []v1alpha1.GroupResources{
							{
								Group:       "",
								Resources:   []string{"secrets"},
								ObjectNames: []string{"controller-secret"},
							},
						},
					},
				},
			},
			internal: &audit.Policy{
				Rules: []audit.PolicyRule{
					{
						Level: audit.LevelRequest,
						OmitStages: []audit.Stage{
							audit.StagePanic,
							audit.StageResponseComplete,
						},
						Users:           []string{"system:kube-proxy"},
						UserGroups:      []string{"system:authenticated"},
						Verbs:           []string{"watch"},
						Namespaces:      []string{"test"},
						NonResourceURLs: []string{"/status"},
						Resources: []audit.GroupResources{
							{
								Group:         "",
								Resources:     []string{"configmaps", "secrets"},
								ResourceNames: []string{"controller-leader"},
							},
						},
					},
					{
						Level:      audit.LevelMetadata,
						UserGroups: []string{"system:authenticated"},
						Verbs:      []string{"patch"},
						Resources: []audit.GroupResources{
							{
								Group:         "",
								Resources:     []string{"secrets"},
								ResourceNames: []string{"controller-secret"},
							},
						},
					},
				},
				OmitStages: []audit.Stage{
					audit.StagePanic,
				},
			},
		},
		{
			desc: "should convert missing stages",
			dynamic: &v1alpha1.Policy{
				Level: v1alpha1.LevelMetadata,
			},
			classes: map[string][]v1alpha1.RequestSelector{},
			internal: &audit.Policy{
				Rules: []audit.PolicyRule{
					{
						Level: audit.LevelMetadata,
					},
				},
				OmitStages: []audit.Stage{
					audit.StageRequestReceived,
					audit.StageResponseStarted,
					audit.StageResponseComplete,
					audit.StagePanic,
				},
			},
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			d := ConvertDynamicPolicyToInternal(test.dynamic, test.classes)
			require.ElementsMatch(t, test.internal.OmitStages, d.OmitStages)
			require.Equal(t, test.internal.Rules, d.Rules)
		})
	}
}
