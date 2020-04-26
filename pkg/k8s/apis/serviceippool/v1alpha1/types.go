// Copyright (c) 2019 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceIPPool contains the specification for an relative between service and ippools.
type ServiceIPPool struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object's metadata.
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Specification of the ServiceIPPool.
	Spec ServiceIPPoolSpec `json:"spec,omitempty"`
}

// ServiceIPPoolSpec contains the enabled and disabled ippools
type ServiceIPPoolSpec struct {
	//enabled ipv4 ippool
	IPv4PoolList []string `json:"ipv4poolList"`
	//disabled ipv4 ippool
	DisabledIPv4PoolList []string `json:"disabledIPv4PoolList"`
	//enabled ipv6 ippool
	IPv6PoolList []string `json:"ipv6poolList"`
	//disabled ipv6 ippool
	DisabledIPv6PoolList []string `json:"disabledIPv6PoolList"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceIPPoolList contains a list of ServiceIPPool resources.
type ServiceIPPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []ServiceIPPool `json:"items"`
}
