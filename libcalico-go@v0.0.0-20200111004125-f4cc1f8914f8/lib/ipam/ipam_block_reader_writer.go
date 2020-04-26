// Copyright (c) 2016-2019 Tigera, Inc. All rights reserved.

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
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/big"
	"math/rand"
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	v3 "github.com/projectcalico/libcalico-go/lib/apis/v3"
	bapi "github.com/projectcalico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/libcalico-go/lib/backend/model"
	cerrors "github.com/projectcalico/libcalico-go/lib/errors"
	cnet "github.com/projectcalico/libcalico-go/lib/net"
)

type blockReaderWriter struct {
	client bapi.Client
	pools  PoolAccessorInterface
}

//获取亲和块、可以释放的亲和块(不属于任何IPPool)
func (rw blockReaderWriter) getAffineBlocks(ctx context.Context, host string, ver int, pools []v3.IPPool) (blocksInPool, blocksNotInPool []cnet.IPNet, err error) {
	blocksInPool = []cnet.IPNet{}
	blocksNotInPool = []cnet.IPNet{}

	// Lookup blocks affine to the specified host.
	opts := model.BlockAffinityListOptions{Host: host, IPVersion: ver}
	//获取blockaffinities crd资源对象
	/*
		apiVersion: crd.projectcalico.org/v1
		kind: BlockAffinity
		metadata:
		  name: 10.10.102.34-slave-192-168-229-64-26
		spec:
		  cidr: 192.168.229.64/26
		  deleted: ""
		  node: 10.10.102.34-slave
		  state: confirmed
	*/
	//查询所有和该host亲和的BlockAffinity资源对象
	datastoreObjs, err := rw.client.List(ctx, opts, "")
	if err != nil {
		if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
			// The block path does not exist yet.  This is OK - it means
			// there are no affine blocks.
			return
		} else {
			log.Errorf("Error getting affine blocks: %v", err)
			return
		}
	}

	// Iterate through and extract the block CIDRs.
	// 遍历并提取块CIDR
	for _, o := range datastoreObjs.KVPairs {
		k := o.Key.(model.BlockAffinityKey)

		// Add the block if no IP pools were specified, or if IP pools were specified
		// and the block falls within the given IP pools.
		if len(pools) == 0 {
			// 未指定IP池,全部按照该BlockAffinity块属于某一个IPPool
			blocksInPool = append(blocksInPool, k.CIDR)
		} else {
			// 指定IP池，则添加块并且该块位于给定的IP池内
			found := false
			for _, pool := range pools {
				var poolNet *cnet.IPNet
				_, poolNet, err = cnet.ParseCIDR(pool.Spec.CIDR)
				if err != nil {
					log.Errorf("Error parsing CIDR: %s from pool: %s %v", pool.Spec.CIDR, pool.Name, err)
					return
				}

				if poolNet.Contains(k.CIDR.IPNet.IP) {
					//BlockAffinity属于此次循环的IPPool
					blocksInPool = append(blocksInPool, k.CIDR)
					found = true
					break
				}
			}
			if !found {
				//BlockAffinity不属于任何IPPool
				blocksNotInPool = append(blocksNotInPool, k.CIDR)
			}
		}
	}
	return
}

// findUnclaimedBlock finds a block cidr which does not yet exist within the given list of pools. The provided pools
// should already be sanitized and only enclude existing, enabled pools. Note that the block may become claimed
// between receiving the cidr from this function and attempting to claim the corresponding block as this function
// does not reserve the returned IPNet.
// findUnclaimedBlock查找在给定IPPool列表中尚不存在的cidr块。提供的IPPool应该已经清理过，并且仅包含已启用的现有池。请注意，
// 在此函数接收cidr和尝试声明相应的block之间，该block可能已被声明所有权, 不保留返回的IPNet
func (rw blockReaderWriter) findUnclaimedBlock(ctx context.Context, host string, version int, pools []v3.IPPool, config IPAMConfig) (*cnet.IPNet, error) {
	// pools为该请求使用的IPPool并且存在且enable的池
	// If there are no pools, we cannot assign addresses.
	// 如果没有IPPool，我们将无法分配地址
	if len(pools) == 0 {
		return nil, fmt.Errorf("no configured Calico pools for node %s", host)
	}

	// Iterate through pools to find a new block.
	// 遍历池以查找新块
	for _, pool := range pools {
		// Use a block generator to iterate through all of the blocks
		// that fall within the pool.
		// 使用块生成器遍历池中的所有块
		log.Debugf("Looking for blocks in pool %+v", pool)
		// randomBlockGenerator() 生成一个函数，每次调用该函数将随机产生一个 block
		// 遍历每次生成的 block，查看是否已被使用，如果未被使用，则返回对应的 block
		blocks := randomBlockGenerator(pool, host)
		for subnet := blocks(); subnet != nil; subnet = blocks() {
			// Check if a block already exists for this subnet.
			// 检查该子网是否已经存在一个块
			log.Debugf("Getting block: %s", subnet.String())
			//根据cidr查询ipamblocks资源对象
			//比如cidr为192.168.123.192/26,则ipamblocks的name则为192-168-123-192-26
			_, err := rw.queryBlock(ctx, *subnet, "")
			// 如果发生错误
			if err != nil {
				if _, ok := err.(cerrors.ErrorResourceDoesNotExist); ok {
					log.Infof("Found free block: %+v", *subnet)
					//不存在,满足要求; 返回未被使用的cidr
					return subnet, nil
				}
				log.Errorf("Error getting block: %v", err)
				return nil, err
			}
			//已存在,不满足要求
			log.Debugf("Block %s already exists", subnet.String())
		}
	}
	return nil, noFreeBlocksError("No Free Blocks")
}

// getPendingAffinity claims a pending affinity for the given host and subnet. The affinity can then
// be used to claim a block. If an affinity already exists, it will return that affinity.
func (rw blockReaderWriter) getPendingAffinity(ctx context.Context, host string, subnet cnet.IPNet) (*model.KVPair, error) {
	logCtx := log.WithFields(log.Fields{"host": host, "subnet": subnet})
	logCtx.Info("Trying to create affinity in pending state")
	obj := model.KVPair{
		Key:   model.BlockAffinityKey{Host: host, CIDR: subnet},
		Value: &model.BlockAffinity{State: model.StatePending},
	}
	aff, err := rw.client.Create(ctx, &obj)
	if err != nil {
		if _, ok := err.(cerrors.ErrorResourceAlreadyExists); !ok {
			logCtx.WithError(err).Error("Failed to claim affinity")
			return nil, err
		}
		logCtx.Info("Block affinity already exists, getting existing affinity")

		// Get the existing affinity.
		aff, err = rw.queryAffinity(ctx, host, subnet, "")
		if err != nil {
			logCtx.WithError(err).Error("Failed to get existing affinity")
			return nil, err
		}
		logCtx.Info("Got existing affinity")

		// If the affinity has not been confirmed already, mark it as pending.
		if aff.Value.(*model.BlockAffinity).State != model.StateConfirmed {
			logCtx.Infof("Marking existing affinity with current state %s as pending", aff.Value.(*model.BlockAffinity).State)
			aff.Value.(*model.BlockAffinity).State = model.StatePending
			return rw.updateAffinity(ctx, aff)
		}
		logCtx.Info("Existing affinity is already confirmed")
		return aff, nil
	}
	logCtx.Infof("Successfully created pending affinity for block")
	return aff, nil
}

// claimAffineBlock claims the provided block using the given pending affinity. If successful, it will confirm the affinity. If another host
// steals the block, claimAffineBlock will attempt to delete the provided pending affinity.
// ClaimAffineBlock使用给定的未决亲缘关系声明提供的块。 如果成功，它将确认亲和力。 如果另一个主机
//窃取该块，claimAffineBlock将尝试删除提供的未决亲缘关系。
func (rw blockReaderWriter) claimAffineBlock(ctx context.Context, aff *model.KVPair, config IPAMConfig) (*model.KVPair, error) {
	// Pull out relevant fields.
	subnet := aff.Key.(model.BlockAffinityKey).CIDR
	host := aff.Key.(model.BlockAffinityKey).Host
	logCtx := log.WithFields(log.Fields{"host": host, "subnet": subnet})

	// Create the new block.
	affinityKeyStr := "host:" + host
	block := newBlock(subnet)
	block.Affinity = &affinityKeyStr
	block.StrictAffinity = config.StrictAffinity

	// Create the new block in the datastore.
	//创建ipamblocks资源对象
	o := model.KVPair{
		Key:   model.BlockKey{CIDR: block.CIDR},
		Value: block.AllocationBlock,
	}
	logCtx.Info("Attempting to create a new block")
	kvp, err := rw.client.Create(ctx, &o)
	if err != nil {
		if _, ok := err.(cerrors.ErrorResourceAlreadyExists); ok {
			// Block already exists, check affinity.
			//创建ipamblocks资源对象时报已存在的错误
			logCtx.Info("The block already exists, getting it from data store")
			//根据传入的cidr查询该已存在的ipamblocks资源对象
			obj, err := rw.queryBlock(ctx, subnet, "")
			if err != nil {
				// We failed to create the block, but the affinity still exists. We don't know
				// if someone else beat us to the block since we can't get it.
				//我们未能创建该块，但亲和力仍然存在。 因为我们无法查到它，我们不知道是否有人击败我们
				logCtx.WithError(err).Errorf("Error reading block")
				return nil, err
			}

			// Pull out the allocationBlock object.
			b := allocationBlock{obj.Value.(*model.AllocationBlock)}

			//如果上面查到的ipamblocks——obj的spec.affinity参数和传入的blockaffinities的spec.node是一致的
			if b.Affinity != nil && *b.Affinity == affinityKeyStr {
				// Block has affinity to this host, meaning another
				// process on this host claimed it. Confirm the affinity
				// and return the existing block.
				// Block与该主机具有亲和力，这意味着该主机上的另一个进程声明了它。 确认关联并返回现有块
				logCtx.Info("Block is already claimed by this host, confirm the affinity")
				//把blockaffinities资源对象的spec.state更新为confirmed
				if _, err := rw.confirmAffinity(ctx, aff); err != nil {
					return nil, err
				}
				return obj, nil
			}

			// Some other host beat us to this block.  Cleanup and return an error.
			// 其他一些host人击败了我们。 清理并返回错误
			log.Info("Block is owned by another host, delete our pending affinity")
			if err = rw.deleteAffinity(ctx, aff); err != nil {
				// Failed to clean up our claim to this block.
				// 无法清理我们申明的该block
				logCtx.WithError(err).Errorf("Error deleting block affinity")
			}
			return nil, errBlockClaimConflict{Block: b}
		}
		logCtx.WithError(err).Warningf("Problem creating block while claiming block")
		return nil, err
	}

	// We've successfully claimed the block - confirm the affinity.
	// 我们已成功声明该 block -确认亲和性
	log.Info("Successfully created block")
	if _, err = rw.confirmAffinity(ctx, aff); err != nil {
		return nil, err
	}
	return kvp, nil
}

func (rw blockReaderWriter) confirmAffinity(ctx context.Context, aff *model.KVPair) (*model.KVPair, error) {
	host := aff.Key.(model.BlockAffinityKey).Host
	cidr := aff.Key.(model.BlockAffinityKey).CIDR
	logCtx := log.WithFields(log.Fields{"host": host, "subnet": cidr})
	logCtx.Info("Confirming affinity")
	aff.Value.(*model.BlockAffinity).State = model.StateConfirmed
	//把blockaffinities资源对象的spec.state更新为confirmed
	confirmed, err := rw.updateAffinity(ctx, aff)
	if err != nil {
		// We couldn't confirm the block - check to see if it was confirmed by
		// another process.
		//我们无法确认该区块-请检查该区块是否已被其他进程确认
		kvp, err2 := rw.queryAffinity(ctx, host, cidr, "")
		if err2 == nil && kvp.Value.(*model.BlockAffinity).State == model.StateConfirmed {
			// Confirmed by someone else - we can use this.
			// 由其他人确认-我们可以使用它
			logCtx.Info("Affinity is already confirmed")
			return kvp, nil
		}
		logCtx.WithError(err).Error("Failed to confirm block affinity")
		return nil, err
	}
	logCtx.Info("Successfully confirmed affinity")
	return confirmed, nil
}

// releaseBlockAffinity releases the host's affinity to the given block, and returns an affinityClaimedError if
// the host does not claim an affinity for the block.
// releaseBlockAffinity释放主机对给定块的亲和力，如果主机不声明对该块的亲和性，则返回affinityClaimedError
func (rw blockReaderWriter) releaseBlockAffinity(ctx context.Context, host string, blockCIDR cnet.IPNet, requireEmpty bool) error {
	// Make sure hostname is not empty.
	if host == "" {
		log.Errorf("Hostname can't be empty")
		return errors.New("Hostname must be sepcified to release block affinity")
	}

	// Read the model.KVPair containing the block affinity.
	logCtx := log.WithFields(log.Fields{"host": host, "subnet": blockCIDR.String()})
	logCtx.Debugf("Attempt to release affinity for block")
	// 根据host和CIDR查询BlockAffinity
	aff, err := rw.queryAffinity(ctx, host, blockCIDR, "")
	if err != nil {
		logCtx.WithError(err).Errorf("Error getting block affinity %s", blockCIDR.String())
		return err
	}

	// Read the model.KVPair containing the block
	// and pull out the allocationBlock object.  We need to hold on to this
	// so that we can pass it back to the datastore on Update.
	//读取包含块的model.KVPair并拉出distributionBlock对象。 我们需要坚持这一点，以便我们可以将其传递回Update上的数据存储
	//根据cidr，查询ipamblock资源对象，比如cidr为192.168.123.192/26,则ipamblocks的name则为192-168-123-192-26
	obj, err := rw.queryBlock(ctx, blockCIDR, "")
	if err != nil {
		logCtx.WithError(err).Warnf("Error getting block")
		return err
	}
	b := allocationBlock{obj.Value.(*model.AllocationBlock)}

	// Check that the block affinity matches the given affinity.
	//检查块关联性是否与给定的关联性匹配
	if b.Affinity != nil && !hostAffinityMatches(host, b.AllocationBlock) {
		// This means the affinity is stale - we can delete it.
		// 这表示亲和性已过时-我们可以将其删除
		logCtx.Errorf("Mismatched affinity: %s != %s - try to delete stale affinity", *b.Affinity, "host:"+host)
		if err := rw.deleteAffinity(ctx, aff); err != nil {
			logCtx.Warn("Failed to delete stale affinity")
		}
		return errBlockClaimConflict{Block: b}
	}

	// Don't release block affinity if we require it to be empty and it's not empty.
	//如果我们要求块亲和性不为空且不为空，则不要释放块相似性
	if requireEmpty && !b.empty() {
		logCtx.Info("Block must be empty but is not empty, refusing to remove affinity.")
		return errBlockNotEmpty{Block: b}
	}

	// Mark the affinity as pending deletion.
	//将ipamblocks资源对象标记为待删除
	aff.Value.(*model.BlockAffinity).State = model.StatePendingDeletion
	aff, err = rw.updateAffinity(ctx, aff)
	if err != nil {
		logCtx.WithError(err).Warnf("Failed to mark block affinity as pending deletion")
		return err
	}

	if b.empty() {
		// If the block is empty, we can delete it.
		// 如果该块为空，我们可以将其删除
		logCtx.Debug("Block is empty - delete it")
		//删除ipamblocks资源对象
		err := rw.deleteBlock(ctx, obj)
		if err != nil {
			if _, ok := err.(cerrors.ErrorResourceDoesNotExist); !ok {
				logCtx.WithError(err).Error("Error deleting block")
				return err
			}
			logCtx.Debug("Block has already been deleted, carry on")
		}
	} else {
		// Otherwise, we need to remove affinity from it.
		// This prevents the host from automatically assigning
		// from this block unless we're allowed to overflow into
		// non-affine blocks.
		// 否则，我们需要从中移除亲和关系。除非允许我们溢出到非亲缘关系块中，否则这将阻止主机从该块中自动分配
		logCtx.Debug("Block is not empty - remove the affinity")
		b.Affinity = nil

		// Pass back the original KVPair with the new
		// block information so we can do a CAS.
		// 回传原始的KVPair与新的ipamblocks信息，以便我们执行CAS
		obj.Value = b.AllocationBlock
		//将ipamblocks资源对象的spec.affinity更新为nil
		_, err = rw.updateBlock(ctx, obj)
		if err != nil {
			logCtx.WithError(err).Error("Failed to remove affinity from block")
			return err
		}
	}

	// We've removed / updated the block, so perform a compare-and-delete on the BlockAffinity.
	//我们已经删除/更新了该块，因此对BlockAffinity进行比较并删除
	if err := rw.deleteAffinity(ctx, aff); err != nil {
		// Return the error unless the affinity didn't exist.
		// 除非亲和力不存在，否则返回错误
		if _, ok := err.(cerrors.ErrorResourceDoesNotExist); !ok {
			logCtx.Errorf("Error deleting block affinity: %v", err)
			return err
		}
	}
	return nil
}

// queryAffinity gets an affinity for the given host + CIDR key.
// queryAffinity获取给定主机+ CIDR key的亲和性块BlockAffinity资源对象
func (rw blockReaderWriter) queryAffinity(ctx context.Context, host string, cidr cnet.IPNet, revision string) (*model.KVPair, error) {
	return rw.client.Get(ctx, model.BlockAffinityKey{Host: host, CIDR: cidr}, revision)
}

// updateAffinity updates the given affinity. BlockAffinity资源对象
func (rw blockReaderWriter) updateAffinity(ctx context.Context, aff *model.KVPair) (*model.KVPair, error) {
	return rw.client.Update(ctx, aff)
}

// deleteAffinity deletes the given affinity.
//deleteAffinity删除给定的亲和 BlockAffinity资源对象
func (rw blockReaderWriter) deleteAffinity(ctx context.Context, aff *model.KVPair) error {
	_, err := rw.client.DeleteKVP(ctx, aff)
	return err
}

// queryBlock gets a block for the given block CIDR key.
// 根据给定的CIDR key查找ipamblocks资源对象
func (rw blockReaderWriter) queryBlock(ctx context.Context, blockCIDR cnet.IPNet, revision string) (*model.KVPair, error) {
	return rw.client.Get(ctx, model.BlockKey{CIDR: blockCIDR}, revision)
}

// updateBlock updates the given block.
// ipamblocks资源对象
func (rw blockReaderWriter) updateBlock(ctx context.Context, b *model.KVPair) (*model.KVPair, error) {
	return rw.client.Update(ctx, b)
}

// deleteBlock deletes the given block.
// ipamblocks资源对象
func (rw blockReaderWriter) deleteBlock(ctx context.Context, b *model.KVPair) error {
	_, err := rw.client.DeleteKVP(ctx, b)
	return err
}

// queryHandle gets a handle for the given handleID key.
func (rw blockReaderWriter) queryHandle(ctx context.Context, handleID, revision string) (*model.KVPair, error) {
	return rw.client.Get(ctx, model.IPAMHandleKey{HandleID: handleID}, revision)
}

// updateHandle updates the given handle.
func (rw blockReaderWriter) updateHandle(ctx context.Context, kvp *model.KVPair) (*model.KVPair, error) {
	return rw.client.Update(ctx, kvp)
}

// deleteHandle deletes the given handle.
func (rw blockReaderWriter) deleteHandle(ctx context.Context, kvp *model.KVPair) error {
	_, err := rw.client.DeleteKVP(ctx, kvp)
	return err
}

// getPoolForIP returns the pool if the given IP is within a configured
// Calico pool, and nil otherwise.
// 如果给定IP在配置的pool中，getPoolForIP返回calico pool，否则返回nil。
func (rw blockReaderWriter) getPoolForIP(ip cnet.IP, enabledPools []v3.IPPool) (*v3.IPPool, error) {
	if enabledPools == nil {
		var err error
		enabledPools, err = rw.pools.GetEnabledPools(ip.Version())
		if err != nil {
			return nil, err
		}
	}
	for _, p := range enabledPools {
		// Compare any enabled pools.
		// 将转入的IP和enable的所有pool比较;返回包含该IP的Pool
		_, pool, err := cnet.ParseCIDR(p.Spec.CIDR)
		if err != nil {
			fields := log.Fields{"pool": p.Name, "cidr": p.Spec.CIDR}
			log.WithError(err).WithFields(fields).Warn("Pool has invalid CIDR")
		} else if pool.Contains(ip.IP) {
			return &p, nil
		}
	}
	return nil, nil
}

// Generator to get list of block CIDRs which
// fall within the given cidr. The passed in pool
// must contain the passed in block cidr.
// Returns nil when no more blocks can be generated.
func blockGenerator(pool *v3.IPPool, cidr cnet.IPNet) func() *cnet.IPNet {
	ip := cnet.IP{IP: cidr.IP}

	var blockMask net.IPMask
	if ip.Version() == 4 {
		blockMask = net.CIDRMask(pool.Spec.BlockSize, 32)
	} else {
		blockMask = net.CIDRMask(pool.Spec.BlockSize, 128)
	}

	ones, size := blockMask.Size()
	blockSize := new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(size-ones)), nil)

	return func() *cnet.IPNet {
		returnIP := ip

		if cidr.Contains(ip.IP) {
			ipnet := net.IPNet{IP: returnIP.IP, Mask: blockMask}
			cidr := cnet.IPNet{IPNet: ipnet}
			ip = cnet.IncrementIP(ip, blockSize)
			return &cidr
		} else {
			return nil
		}
	}
}

func determineSeed(mask net.IPMask, hostname string) int64 {
	if ones, bits := mask.Size(); ones == bits {
		// For small blocks, we don't care about the same host picking the same
		// block, so just use a seed based on timestamp. This optimization reduces
		// the number of reads required to find an unclaimed block on a host.
		return time.Now().UTC().UnixNano()
	}

	// Create a random number generator seed based on the hostname.
	// This is to avoid assigning multiple blocks when multiple
	// workloads request IPs around the same time.
	hostHash := fnv.New32()
	hostHash.Write([]byte(hostname))
	return int64(hostHash.Sum32())
}

// Returns a generator that, when called, returns a random
// block from the given pool.  When there are no blocks left,
// the it returns nil.
func randomBlockGenerator(ipPool v3.IPPool, hostName string) func() *cnet.IPNet {
	_, pool, err := cnet.ParseCIDR(ipPool.Spec.CIDR)
	if err != nil {
		log.Errorf("Error parsing CIDR: %s %v", ipPool.Spec.CIDR, err)
		return func() *cnet.IPNet { return nil }
	}

	// Determine the IP type to use.
	baseIP := cnet.IP{IP: pool.IP}
	version := getIPVersion(baseIP)
	var blockMask net.IPMask
	if version == 4 {
		blockMask = net.CIDRMask(ipPool.Spec.BlockSize, 32)
	} else {
		blockMask = net.CIDRMask(ipPool.Spec.BlockSize, 128)
	}

	// Determine the number of blocks within this pool.
	ones, size := pool.Mask.Size()
	numIP := new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(size-ones)), nil)

	ones, size = blockMask.Size()
	blockSize := new(big.Int).Exp(big.NewInt(2), big.NewInt(int64(size-ones)), nil)

	numBlocks := new(big.Int)
	numBlocks.Div(numIP, blockSize)

	// Build a random number generator.
	seed := determineSeed(blockMask, hostName)
	randm := rand.New(rand.NewSource(seed))

	// initialIndex keeps track of the random starting point
	initialIndex := new(big.Int)
	initialIndex.Rand(randm, numBlocks)

	// i keeps track of current index while walking the blocks in a pool
	i := initialIndex

	// numReturned keeps track of number of blocks returned
	numReturned := big.NewInt(0)

	// numDiff = numBlocks - i
	numDiff := new(big.Int)

	return func() *cnet.IPNet {
		// The `big.NewInt(0)` part creates a temp variable and assigns the result of multiplication of `i` and `big.NewInt(blockSize)`
		// Note: we are not using `i.Mul()` because that will assign the result of the multiplication to `i`, which will cause unexpected issues
		ip := cnet.IncrementIP(baseIP, big.NewInt(0).Mul(i, blockSize))

		ipnet := net.IPNet{ip.IP, blockMask}

		numDiff.Sub(numBlocks, i)

		if numDiff.Cmp(big.NewInt(1)) <= 0 {
			// Index has reached end of the blocks;
			// Loop back to beginning of pool rather than
			// increment, because incrementing would put us outside of the pool.
			i = big.NewInt(0)
		} else {
			// Increment to the next block
			i.Add(i, big.NewInt(1))
		}

		if numReturned.Cmp(numBlocks) >= 0 {
			// Index finished one full circle across the blocks
			// Used all of the blocks in this pool.
			//索引在块上完成了一个完整的圆圈// //使用了该池中的所有块。
			//numReturned>numBlocks
			return nil
		}
		numReturned.Add(numReturned, big.NewInt(1))

		// Return the block from this pool that corresponds with the index.
		return &cnet.IPNet{ipnet}
	}
}

// Find the block for a given IP (without needing a pool)
func (rw blockReaderWriter) getBlockForIP(ctx context.Context, ip cnet.IP) (*cnet.IPNet, error) {
	// Lookup all blocks by providing an empty BlockListOptions to the List operation.
	opts := model.BlockListOptions{IPVersion: ip.Version()}
	datastoreObjs, err := rw.client.List(ctx, opts, "")
	if err != nil {
		log.Errorf("Error getting affine blocks: %v", err)
		return nil, err
	}

	// Iterate through and extract the block CIDRs.
	for _, o := range datastoreObjs.KVPairs {
		k := o.Key.(model.BlockKey)
		if k.CIDR.IPNet.Contains(ip.IP) {
			log.Debugf("Found IP %s in block %s", ip.String(), k.String())
			return &k.CIDR, nil
		}
	}

	// No blocks found.
	log.Debugf("IP %s could not be found in any blocks", ip.String())
	return nil, nil
}
