// Copyright (c) 2016-2017 Tigera, Inc. All rights reserved.

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

package ipam

import (
	"errors"
	"fmt"

	"github.com/projectcalico/libcalico-go/lib/backend/model"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
)

type allocationHandle struct {
	*model.IPAMHandle
}

func (h allocationHandle) incrementBlock(blockCidr cnet.IPNet, num int) int {
	blockId := blockCidr.String()
	newNum := num
	if val, ok := h.Block[blockId]; ok {
		// An entry exists for this block, increment the number
		// of allocations.
		newNum = val + num
	}
	h.Block[blockId] = newNum
	return newNum
}

//递减ipamhandles.spec.block
/*
apiVersion: crd.projectcalico.org/v1
kind: IPAMHandle
metadata:
  name: k8s-pod-network.a78d2a5bda51f24338408ea3c08788ffe5bd9f0b942f0682d3ddf4202ae7200f
spec:
  block:
    192.168.198.0/26: 1
  handleID: k8s-pod-network.a78d2a5bda51f24338408ea3c08788ffe5bd9f0b942f0682d3ddf4202ae7200f
*/
func (h allocationHandle) decrementBlock(blockCidr cnet.IPNet, num int) (*int, error) {
	blockId := blockCidr.String()
	if current, ok := h.Block[blockId]; !ok {
		// This entry doesn't exist.
		errStr := fmt.Sprintf("Tried to decrement block %s by %v but it isn't linked to handle %s", blockId, num, h.HandleID)
		return nil, errors.New(errStr)
	} else {
		newNum := current - num
		if newNum < 0 {
			errStr := fmt.Sprintf("Tried to decrement block %s by %v but it only has %v addresses on handle %s", blockId, num, current, h.HandleID)
			return nil, errors.New(errStr)
		}

		if newNum == 0 {
			//从map中删除该cidr的key
			delete(h.Block, blockId)
		} else {
			//不==0时，map的value减去释放的IP数
			h.Block[blockId] = newNum
		}
		return &newNum, nil
	}
}

func (h allocationHandle) empty() bool {
	return len(h.Block) == 0
}
