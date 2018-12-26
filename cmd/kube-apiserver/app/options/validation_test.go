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

package options

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	utilnet "k8s.io/apimachinery/pkg/util/net"
)

func Test_validateServiceNodePort(t *testing.T) {

	t.Run("valid configuration", func(t *testing.T) {
		validOptions := &ServerRunOptions{
			KubernetesServiceNodePort: 8443,
			KubernetesServicePort:     9443,
			ServiceNodePortRange:      *utilnet.ParsePortRangeOrDie("8000-9000"),
		}

		assert.Empty(t, validateServiceNodePort(validOptions), "unexpected error has occured")
	})

	tests := []struct {
		name   string
		ops    *ServerRunOptions
		errors []error
	}{
		{
			"kubernetes-service-node-port lower than 0",
			&ServerRunOptions{
				KubernetesServiceNodePort: -1,
				KubernetesServicePort:     9443,
				ServiceNodePortRange:      *utilnet.ParsePortRangeOrDie("8000-9000"),
			},
			[]error{
				errors.New("--kubernetes-service-node-port -1 must be between 0 and 65535, inclusive. If 0, the Kubernetes master service will be of type ClusterIP"),
			},
		},
		{
			"kubernetes-service-node-port higher than 65535",
			&ServerRunOptions{
				KubernetesServiceNodePort: 65536,
				KubernetesServicePort:     9443,
				ServiceNodePortRange:      *utilnet.ParsePortRangeOrDie("8000-65535"),
			},
			[]error{
				errors.New("--kubernetes-service-node-port 65536 must be between 0 and 65535, inclusive. If 0, the Kubernetes master service will be of type ClusterIP"),
				errors.New("kubernetes service port range 8000-65535 doesn't contain 65536"),
			},
		},
		{
			"kubernetes-service-node-port not in port range",
			&ServerRunOptions{
				KubernetesServiceNodePort: 443,
				KubernetesServicePort:     9443,
				ServiceNodePortRange:      *utilnet.ParsePortRangeOrDie("1-10"),
			},
			[]error{
				errors.New("kubernetes service port range 1-10 doesn't contain 443"),
			},
		},
		{
			"kubernetes-service-port lower than 1",
			&ServerRunOptions{
				KubernetesServiceNodePort: 8443,
				KubernetesServicePort:     0,
				ServiceNodePortRange:      *utilnet.ParsePortRangeOrDie("8000-9000"),
			},
			[]error{errors.New("--kubernetes-service-port 0 must be between 1 and 65535, inclusive")},
		},
		{
			"kubernetes-service-port higher than 65535",
			&ServerRunOptions{
				KubernetesServiceNodePort: 8443,
				KubernetesServicePort:     65536,
				ServiceNodePortRange:      *utilnet.ParsePortRangeOrDie("8000-65535"),
			},
			[]error{
				errors.New("--kubernetes-service-port 65536 must be between 1 and 65535, inclusive"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateServiceNodePort(tt.ops)

			assert.ElementsMatch(t, err, tt.errors)
		})
	}
}
