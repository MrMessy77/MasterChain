// Copyright 2017 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package ethash

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/EDXFund/MasterChain/log"
	"math/big"
	"reflect"
	"runtime"
	"time"

	"github.com/EDXFund/MasterChain/common"
	"github.com/EDXFund/MasterChain/common/math"
	"github.com/EDXFund/MasterChain/consensus"
	"github.com/EDXFund/MasterChain/consensus/misc"
	"github.com/EDXFund/MasterChain/core/state"
	"github.com/EDXFund/MasterChain/core/types"
	"github.com/EDXFund/MasterChain/crypto/sha3"
	"github.com/EDXFund/MasterChain/params"
	"github.com/EDXFund/MasterChain/rlp"
	mapset "github.com/deckarep/golang-set"
)

// Ethash proof-of-work protocol constants.
var (
	FrontierBlockReward       = big.NewInt(5e+18) // Block reward in wei for successfully mining a block
	ByzantiumBlockReward      = big.NewInt(3e+18) // Block reward in wei for successfully mining a block upward from Byzantium
	ConstantinopleBlockReward = big.NewInt(2e+18) // Block reward in wei for successfully mining a block upward from Constantinople
	maxUncles                 = 2                 // Maximum number of uncles allowed in a single block
	allowedFutureBlockTime    = 15 * time.Second  // Max time from current time allowed for blocks, before they're considered future blocks

	// calcDifficultyConstantinople is the difficulty adjustment algorithm for Constantinople.
	// It returns the difficulty that a new block should have when created at time given the
	// parent block's time and difficulty. The calculation uses the Byzantium rules, but with
	// bomb offset 5M.
	// Specification EIP-1234: https://eips.ethereum.org/EIPS/eip-1234
	calcDifficultyConstantinople = makeDifficultyCalculator(big.NewInt(5000000))

	// calcDifficultyByzantium is the difficulty adjustment algorithm. It returns
	// the difficulty that a new block should have when created at time given the
	// parent block's time and difficulty. The calculation uses the Byzantium rules.
	// Specification EIP-649: https://eips.ethereum.org/EIPS/eip-649
	calcDifficultyByzantium = makeDifficultyCalculator(big.NewInt(3000000))
)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	errLargeBlockTime    = errors.New("timestamp too big")
	errZeroBlockTime     = errors.New("timestamp equals parent's")
	errTooManyUncles     = errors.New("too many uncles")
	errDuplicateUncle    = errors.New("duplicate uncle")
	errUncleIsAncestor   = errors.New("uncle is ancestor")
	errDanglingUncle     = errors.New("uncle's parent is not ancestor")
	errInvalidDifficulty = errors.New("non-positive difficulty")
	errInvalidMixDigest  = errors.New("invalid mix digest")
	errInvalidPoW        = errors.New("invalid proof-of-work")
)

// Author implements consensus.Engine, returning the header's coinbase as the
// proof-of-work verified author of the block.
func (ethash *Ethash) Author(header types.HeaderIntf) (common.Address, error) {
	return header.Coinbase(), nil
}

// VerifyHeader checks whether a header conforms to the consensus rules of the
// stock Ethereum ethash engine.
func (ethash *Ethash) VerifyHeader(chain consensus.ChainReader, header types.HeaderIntf, seal bool) error {
	// If we're running a full engine faking, accept any input as valid
	if ethash.config.PowMode == ModeFullFake {
		return nil
	}
	// Short circuit if the header is known, or it's parent not
	number := header.NumberU64()
	if chain.GetHeader(header.Hash(), number) != nil {
		return nil
	}
	parent := chain.GetHeader(header.ParentHash(), number-1)
	if parent == nil || reflect.ValueOf(parent).IsNil() {

		return consensus.ErrUnknownAncestor
	}
	// Sanity checks passed, do a proper verification
	////must to do adapt to shard block
	return ethash.verifyHeader(chain, header, parent, false, seal)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications.
func (ethash *Ethash) VerifyHeaders(chain consensus.ChainReader, headers []types.HeaderIntf, seals []bool) (chan<- struct{}, <-chan error) {
	// If we're running a full engine faking, accept any input as valid
	if ethash.config.PowMode == ModeFullFake || len(headers) == 0 {
		abort, results := make(chan struct{}), make(chan error, len(headers))
		for i := 0; i < len(headers); i++ {
			results <- nil
		}
		return abort, results
	}

	// Spawn as many workers as allowed threads
	workers := runtime.GOMAXPROCS(0)
	if len(headers) < workers {
		workers = len(headers)
	}

	// Create a task channel and spawn the verifiers
	var (
		inputs = make(chan int)
		done   = make(chan int, workers)
		errors = make([]error, len(headers))
		abort  = make(chan struct{})
	)
	for i := 0; i < workers; i++ {
		go func() {
			for index := range inputs {
				errors[index] = ethash.verifyHeaderWorker(chain, headers, seals, index)
				done <- index
			}
		}()
	}

	errorsOut := make(chan error, len(headers))
	go func() {
		defer close(inputs)
		var (
			in, out = 0, 0
			checked = make([]bool, len(headers))
			inputs  = inputs
		)
		for {
			select {
			case inputs <- in:
				if in++; in == len(headers) {
					// Reached end of headers. Stop sending to workers.
					inputs = nil
				}
			case index := <-done:
				for checked[index] = true; checked[out]; out++ {
					errorsOut <- errors[out]
					if out == len(headers)-1 {
						return
					}
				}
			case <-abort:
				return
			}
		}
	}()
	return abort, errorsOut
}

func (ethash *Ethash) verifyHeaderWorker(chain consensus.ChainReader, headers []types.HeaderIntf, seals []bool, index int) error {
	var parent types.HeaderIntf
	if index == 0 {
		parent = chain.GetHeader(headers[0].ParentHash(), headers[0].NumberU64()-1)
	} else if headers[index-1].Hash() == headers[index].ParentHash() {
		parent = headers[index-1]
	}
	if parent == nil || reflect.ValueOf(parent).IsNil() {
		log.Debug("error in find parent:", "index:", index, "number:", headers[index].NumberU64(), "hash:", headers[index].Hash(), "parentHash:", headers[index].ParentHash())
		//	fmt.Println("")
		return consensus.ErrUnknownAncestor
	} else {
		log.Trace("in find parent:", "index:", index, "number:", headers[index].NumberU64(), "hash:", headers[index].Hash(), "parentHash:", headers[index].ParentHash())
		//	fmt.Println("")

	}
	if chain.GetHeader(headers[index].Hash(), headers[index].NumberU64()) != nil {
		return nil // known block
	}
	//fmt.Println("header:",headers[index].NumberU64(),"\tparent:",parent.NumberU64())
	return ethash.verifyHeader(chain, headers[index], parent, false, seals[index])
}

// VerifyUncles verifies that the given block's uncles conform to the consensus
// rules of the stock Ethereum ethash engine.
func (ethash *Ethash) VerifyUncles(chain consensus.ChainReader, block types.BlockIntf) error {
	// If we're running a full engine faking, accept any input as valid
	if ethash.config.PowMode == ModeFullFake {
		return nil
	}
	// Verify that there are at most 2 uncles included in this block
	if len(block.Uncles()) > maxUncles {
		return errTooManyUncles
	}
	// Gather the set of past uncles and ancestors
	uncles, ancestors := mapset.NewSet(), make(map[common.Hash]types.HeaderIntf)

	number, parent := block.NumberU64()-1, block.ParentHash()
	for i := 0; i < 7; i++ {
		ancestor := chain.GetBlock(parent, number)
		if ancestor == nil || reflect.ValueOf(ancestor).IsNil() {
			break
		}
		ancestors[ancestor.Hash()] = ancestor.Header()
		for _, uncle := range ancestor.Uncles() {
			uncles.Add(uncle.Hash())
		}
		parent, number = ancestor.ParentHash(), number-1
	}
	ancestors[block.Hash()] = block.Header()
	uncles.Add(block.Hash())

	// Verify each of the uncles that it's recent, but not an ancestor
	for _, uncle := range block.Uncles() {
		// Make sure every uncle is rewarded only once
		hash := uncle.Hash()
		if uncles.Contains(hash) {
			return errDuplicateUncle
		}
		uncles.Add(hash)

		// Make sure the uncle has a valid ancestry
		if ancestors[hash] != nil {
			return errUncleIsAncestor
		}
		val := ancestors[uncle.ParentHash()]
		if val == nil || reflect.ValueOf(val).IsNil() || uncle.ParentHash() == block.ParentHash() {
			return errDanglingUncle
		}
		if err := ethash.verifyHeader(chain, uncle, ancestors[uncle.ParentHash()], true, true); err != nil {
			return err
		}
	}
	return nil
}

// verifyHeader checks whether a header conforms to the consensus rules of the
// stock Ethereum ethash engine.
// See YP section 4.3.4. "Block Header Validity"
func (ethash *Ethash) verifyHeader(chain consensus.ChainReader, header, parent types.HeaderIntf, uncle bool, seal bool) error {
	// Ensure that the header's extra-data section is of a reasonable size
	if uint64(len(header.Extra())) > params.MaximumExtraDataSize {
		return fmt.Errorf("extra-data too long: %d > %d", len(header.Extra()), params.MaximumExtraDataSize)
	}
	// Verify the header's timestamp
	if uncle {
		if header.Time().Cmp(math.MaxBig256) > 0 {
			return errLargeBlockTime
		}
	} else {
		if header.Time().Cmp(big.NewInt(time.Now().Add(allowedFutureBlockTime).Unix())) > 0 {
			return consensus.ErrFutureBlock
		}
	}
	if header.Time().Cmp(parent.Time()) <= 0 {
		return errZeroBlockTime
	}
	// Verify the block's difficulty based in it's timestamp and parent's difficulty

	expected := ethash.CalcDifficulty(chain, header.Time().Uint64(), parent)

	if expected.Cmp(header.Difficulty()) != 0 {
		return fmt.Errorf("invalid difficulty: have %v, want %v", header.Difficulty(), expected)
	}
	// Verify that the gas limit is <= 2^63-1
	cap := uint64(0x7fffffffffffffff)
	if header.GasLimit() > cap {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit(), cap)
	}
	// Verify that the gasUsed is <= gasLimit
	if header.GasUsed() > header.GasLimit() {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed(), header.GasLimit())
	}

	// Verify that the gas limit remains within allowed bounds
	diff := int64(parent.GasLimit()) - int64(header.GasLimit())
	if diff < 0 {
		diff *= -1
	}
	limit := parent.GasLimit() / params.GasLimitBoundDivisor

	if uint64(diff) >= limit || header.GasLimit() < params.MinGasLimit {
		return fmt.Errorf("invalid gas limit: have %d, want %d += %d", header.GasLimit(), parent.GasLimit(), limit)
	}
	// Verify that the block number is parent's +1
	if diff := new(big.Int).Sub(header.Number(), parent.Number()); diff.Cmp(big.NewInt(1)) != 0 {
		return consensus.ErrInvalidNumber
	}
	// Verify the engine specific seal securing the block
	if seal {
		if err := ethash.VerifySeal(chain, header); err != nil {
			return err
		}
	}
	// If all checks passed, validate any special fields for hard forks
	if err := misc.VerifyDAOHeaderExtraData(chain.Config(), header); err != nil {
		return err
	}
	if err := misc.VerifyForkHashes(chain.Config(), header, uncle); err != nil {
		return err
	}
	return nil
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns
// the difficulty that a new block should have when created at time
// given the parent block's time and difficulty.
func (ethash *Ethash) CalcDifficulty(chain consensus.ChainReader, time uint64, parent types.HeaderIntf) *big.Int {
	return CalcDifficulty(chain.Config(), time, parent)
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns
// the difficulty that a new block should have when created at time
// given the parent block's time and difficulty.
func CalcDifficulty(config *params.ChainConfig, time uint64, parent types.HeaderIntf) *big.Int {
	next := new(big.Int).Add(parent.Number(), big1)
	switch {
	case config.IsConstantinople(next):
		return calcDifficultyConstantinople(time, parent)
	case config.IsByzantium(next):
		return calcDifficultyByzantium(time, parent)
	case config.IsHomestead(next):
		return calcDifficultyHomestead(time, parent)
	default:
		return calcDifficultyFrontier(time, parent)
	}
}

// Some weird constants to avoid constant memory allocs for them.
var (
	expDiffPeriod = big.NewInt(100000)
	big1          = big.NewInt(1)
	big2          = big.NewInt(2)
	big9          = big.NewInt(9)
	big10         = big.NewInt(10)
	bigMinus99    = big.NewInt(-99)
)

// makeDifficultyCalculator creates a difficultyCalculator with the given bomb-delay.
// the difficulty is calculated with Byzantium rules, which differs from Homestead in
// how uncles affect the calculation
func makeDifficultyCalculator(bombDelay *big.Int) func(time uint64, parent types.HeaderIntf) *big.Int {
	// Note, the calculations below looks at the parent number, which is 1 below
	// the block number. Thus we remove one from the delay given

	bombDelayFromParent := new(big.Int).Sub(bombDelay, big1)
	return func(time uint64, parent types.HeaderIntf) *big.Int {
		// https://github.com/ethereum/EIPs/issues/100.
		// algorithm:
		// diff = (parent_diff +
		//         (parent_diff / 2048 * max((2 if len(parent.uncles) else 1) - ((timestamp - parent.timestamp) // 9), -99))
		//        ) + 2^(periodCount - 2)

		bigTime := new(big.Int).SetUint64(time)
		bigParentTime := new(big.Int).Set(parent.Time())

		// holds intermediate values to make the algo easier to read & audit
		x := new(big.Int)
		y := new(big.Int)

		// (2 if len(parent_uncles) else 1) - (block_timestamp - parent_timestamp) // 9
		x.Sub(bigTime, bigParentTime)
		x.Div(x, big9)
		/*if parent.UncleHash() == types.EmptyUncleHash{*/
		x.Sub(big1, x)
		/*} else {
			x.Sub(big2, x)
		}*/
		// max((2 if len(parent_uncles) else 1) - (block_timestamp - parent_timestamp) // 9, -99)
		if x.Cmp(bigMinus99) < 0 {
			x.Set(bigMinus99)
		}
		// parent_diff + (parent_diff / 2048 * max((2 if len(parent.uncles) else 1) - ((timestamp - parent.timestamp) // 9), -99))
		y.Div(parent.Difficulty(), params.DifficultyBoundDivisor)
		x.Mul(y, x)
		x.Add(parent.Difficulty(), x)

		// minimum difficulty can ever be (before exponential factor)
		if x.Cmp(params.MinimumDifficulty) < 0 {
			x.Set(params.MinimumDifficulty)
		}
		// calculate a fake block number for the ice-age delay
		// Specification: https://eips.ethereum.org/EIPS/eip-1234
		fakeBlockNumber := new(big.Int)
		if parent.Number().Cmp(bombDelayFromParent) >= 0 {
			fakeBlockNumber = fakeBlockNumber.Sub(parent.Number(), bombDelayFromParent)
		}
		// for the exponential factor
		periodCount := fakeBlockNumber
		periodCount.Div(periodCount, expDiffPeriod)

		// the exponential factor, commonly referred to as "the bomb"
		// diff = diff + 2^(periodCount - 2)
		if periodCount.Cmp(big1) > 0 {
			y.Sub(periodCount, big2)
			y.Exp(big2, y, nil)
			x.Add(x, y)
		}
		return x
	}
}

// calcDifficultyHomestead is the difficulty adjustment algorithm. It returns
// the difficulty that a new block should have when created at time given the
// parent block's time and difficulty. The calculation uses the Homestead rules.
func calcDifficultyHomestead(time uint64, parent types.HeaderIntf) *big.Int {
	// https://github.com/ethereum/EIPs/blob/master/EIPS/eip-2.md
	// algorithm:
	// diff = (parent_diff +
	//         (parent_diff / 2048 * max(1 - (block_timestamp - parent_timestamp) // 10, -99))
	//        ) + 2^(periodCount - 2)

	bigTime := new(big.Int).SetUint64(time)
	bigParentTime := new(big.Int).Set(parent.Time())

	// holds intermediate values to make the algo easier to read & audit
	x := new(big.Int)
	y := new(big.Int)

	// 1 - (block_timestamp - parent_timestamp) // 10
	x.Sub(bigTime, bigParentTime)
	x.Div(x, big10)
	x.Sub(big1, x)

	// max(1 - (block_timestamp - parent_timestamp) // 10, -99)
	if x.Cmp(bigMinus99) < 0 {
		x.Set(bigMinus99)
	}
	// (parent_diff + parent_diff // 2048 * max(1 - (block_timestamp - parent_timestamp) // 10, -99))
	y.Div(parent.Difficulty(), params.DifficultyBoundDivisor)
	x.Mul(y, x)
	x.Add(parent.Difficulty(), x)

	// minimum difficulty can ever be (before exponential factor)
	if x.Cmp(params.MinimumDifficulty) < 0 {
		x.Set(params.MinimumDifficulty)
	}
	// for the exponential factor
	periodCount := new(big.Int).Add(parent.Number(), big1)
	periodCount.Div(periodCount, expDiffPeriod)

	// the exponential factor, commonly referred to as "the bomb"
	// diff = diff + 2^(periodCount - 2)
	if periodCount.Cmp(big1) > 0 {
		y.Sub(periodCount, big2)
		y.Exp(big2, y, nil)
		x.Add(x, y)
	}

	return x
}

// calcDifficultyFrontier is the difficulty adjustment algorithm. It returns the
// difficulty that a new block should have when created at time given the parent
// block's time and difficulty. The calculation uses the Frontier rules.
func calcDifficultyFrontier(time uint64, parent types.HeaderIntf) *big.Int {
	diff := new(big.Int)
	adjust := new(big.Int).Div(parent.Difficulty(), params.DifficultyBoundDivisor)
	bigTime := new(big.Int)
	bigParentTime := new(big.Int)

	bigTime.SetUint64(time)
	bigParentTime.Set(parent.Time())

	if bigTime.Sub(bigTime, bigParentTime).Cmp(params.DurationLimit) < 0 {
		diff.Add(parent.Difficulty(), adjust)
	} else {
		diff.Sub(parent.Difficulty(), adjust)
	}
	if diff.Cmp(params.MinimumDifficulty) < 0 {
		diff.Set(params.MinimumDifficulty)
	}

	periodCount := new(big.Int).Add(parent.Number(), big1)
	periodCount.Div(periodCount, expDiffPeriod)
	if periodCount.Cmp(big1) > 0 {
		// diff = diff + 2^(periodCount - 2)
		expDiff := periodCount.Sub(periodCount, big2)
		expDiff.Exp(big2, expDiff, nil)
		diff.Add(diff, expDiff)
		diff = math.BigMax(diff, params.MinimumDifficulty)
	}

	return diff
}

// VerifySeal implements consensus.Engine, checking whether the given block satisfies
// the PoW difficulty requirements.
func (ethash *Ethash) VerifySeal(chain consensus.ChainReader, header types.HeaderIntf) error {
	return ethash.verifySeal(chain, header, false)
}

// verifySeal checks whether a block satisfies the PoW difficulty requirements,
// either using the usual ethash cache for it, or alternatively using a full DAG
// to make remote mining fast.
func (ethash *Ethash) verifySeal(chain consensus.ChainReader, header types.HeaderIntf, fulldag bool) error {
	// If we're running a fake PoW, accept any seal as valid
	if ethash.config.PowMode == ModeFake || ethash.config.PowMode == ModeFullFake {
		timer := time.NewTimer(ethash.fakeDelay)
		<-timer.C
		if ethash.fakeFail == header.NumberU64() {
			return errInvalidPoW
		}
		return nil
	}
	// If we're running a shared PoW, delegate verification to it
	if ethash.shared != nil {
		return ethash.shared.verifySeal(chain, header, fulldag)
	}
	// Ensure that we have a valid difficulty for the block
	if header.Difficulty().Sign() <= 0 {
		return errInvalidDifficulty
	}
	// Recompute the digest and PoW values
	number := header.NumberU64()

	var (
		digest []byte
		result []byte
	)
	// If fast-but-heavy PoW verification was requested, use an ethash dataset
	if fulldag {
		dataset := ethash.dataset(number, true)
		if dataset.generated() {
			digest, result = hashimotoFull(dataset.dataset, ethash.SealHash(header).Bytes(), header.Nonce().Uint64())

			// Datasets are unmapped in a finalizer. Ensure that the dataset stays alive
			// until after the call to hashimotoFull so it's not unmapped while being used.
			runtime.KeepAlive(dataset)
		} else {
			// Dataset not yet generated, don't hang, use a cache instead
			fulldag = false
		}
	}
	// If slow-but-light PoW verification was requested (or DAG not yet ready), use an ethash cache
	if !fulldag {
		cache := ethash.cache(number)

		size := datasetSize(number)
		if ethash.config.PowMode == ModeTest {
			size = 32 * 1024
		}
		digest, result = hashimotoLight(size, cache.cache, ethash.SealHash(header).Bytes(), header.Nonce().Uint64())

		// Caches are unmapped in a finalizer. Ensure that the cache stays alive
		// until after the call to hashimotoLight so it's not unmapped while being used.
		runtime.KeepAlive(cache)
	}
	// Verify the calculated values against the ones provided in the header
	digestHeader := header.MixDigest()

	if !bytes.Equal(digestHeader[:], digest) {
		return errInvalidMixDigest
	}
	target := new(big.Int).Div(two256, header.Difficulty())
	if new(big.Int).SetBytes(result).Cmp(target) > 0 {
		return errInvalidPoW
	}
	return nil
}

// Prepare implements consensus.Engine, initializing the difficulty field of a
// header to conform to the ethash protocol. The changes are done inline.
func (ethash *Ethash) Prepare(chain consensus.ChainReader, header types.HeaderIntf) error {
	parent := chain.GetHeader(header.ParentHash(), header.NumberU64()-1)
	if parent == nil || reflect.ValueOf(parent).IsNil() {
		return consensus.ErrUnknownAncestor
	}
	header.SetDifficulty(ethash.CalcDifficulty(chain, header.Time().Uint64(), parent))
	return nil
}

// Finalize implements consensus.Engine, accumulating the block and uncle rewards,
// setting the final state and assembling the block.
func (ethash *Ethash) Finalize(chain consensus.ChainReader, header types.HeaderIntf, state *state.StateDB, blks []*types.ShardBlockInfo, results []*types.ContractResult, txs []*types.Transaction, receipts []*types.Receipt) (types.BlockIntf, error) {
	if header.ShardId() == types.ShardMaster {
		return ethash.finalizeMaster(chain, header, state, blks, receipts)
	} else {
		return ethash.finalizeShard(chain, header, state, results)
	}
}

// Finalize implements consensus.Engine, ensuring no uncles are set, nor block
// rewards given, and returns the final block.
func (ethash *Ethash) finalizeShard(chain consensus.ChainReader, header types.HeaderIntf, state *state.StateDB, results []*types.ContractResult) (types.BlockIntf, error) {
	// Accumulate any block and uncle rewards and commit the final state root
	/*accumulateRewards(chain.Config(), state, header, nil)

	 */
	header.SetRoot(state.IntermediateRoot(chain.Config().IsEIP158(header.Number())))
	// Header seems complete, assemble into a block and return
	return types.NewSBlock(header, results), nil
}
func (ethash *Ethash) finalizeMaster(chain consensus.ChainReader, header types.HeaderIntf, state *state.StateDB, blks []*types.ShardBlockInfo, receipts []*types.Receipt) (types.BlockIntf, error) {
	// Accumulate any block and uncle rewards and commit the final state root
	parent := chain.GetHeader(header.ParentHash(), header.NumberU64()-1)
	accumulateRewards(chain.Config(), state, parent, header, blks)
	header.SetRoot(state.IntermediateRoot(chain.Config().IsEIP158(header.Number())))

	// Header seems complete, assemble into a block and return
	out := make([]types.ShardBlockInfo, len(blks))
	for i, val := range blks {
		out[i] = *val
	}
	//fmt.Println(" shard blocks:",out)
	return types.NewBlock(header, blks, nil, receipts), nil
}

// SealHash returns the hash of a block prior to it being sealed.
func (ethash *Ethash) SealHash(header types.HeaderIntf) (hash common.Hash) {
	hasher := sha3.NewKeccak256()

	if header.ShardId() == types.ShardMaster {
		rlp.Encode(hasher, []interface{}{
			header.ParentHash(),
			header.UncleHash(),
			header.Coinbase(),
			header.Root(),
			header.TxHash(),
			header.ReceiptHash(),
			header.Bloom(),
			header.Difficulty(),
			header.Number(),
			header.GasLimit(),
			header.GasUsed(),
			header.Time(),
			header.Extra(),
		})
	} else {
		rlp.Encode(hasher, []interface{}{
			header.ShardId(),
			header.ParentHash(),
			header.Coinbase(),
			header.Root(),
			header.TxHash(),
			header.ReceiptHash(),
			header.Bloom(),
			header.Difficulty(),
			header.Number(),
			header.GasLimit(),
			header.GasUsed(),
			header.Time(),
			header.Extra(),
		})
	}
	hasher.Sum(hash[:0])
	return hash
}

// Some weird constants to avoid constant memory allocs for them.
var (
	big8  = big.NewInt(8)
	big32 = big.NewInt(32)
)

var bitMask = [8]uint8{0x01, 0x02, 0x04, 0x08, 0x10, 0x20, 0x40, 0x80}
var stageMask = []uint64{13200000, 26400000, 39600000, 52800000, 66000000, 79200000}
var blockRewardBase = new(big.Int).Mul(big.NewInt(1e14), big.NewInt(3125))
var rewardBaseUint = new(big.Int).Mul(big.NewInt(1e10), big.NewInt(3125))

// AccumulateRewards credits the coinbase of the given block with the mining
// reward. The total reward consists of the static block reward and rewards for
// included uncles. The coinbase of each uncle block is also rewarded.
// the more shard included the more main block can be rewarded
func accumulateRewards(config *params.ChainConfig, state *state.StateDB, parent types.HeaderIntf, header types.HeaderIntf, blks []*types.ShardBlockInfo) {
	// Select the correct block reward based on chain progression

	blockRewardBase := new(big.Int).Mul(big.NewInt(1e14), big.NewInt(3125))

	multiple := int64(1)
	blockNumber := header.NumberU64()
	if blockNumber < stageMask[0] {
		multiple = 64
	} else if blockNumber >= stageMask[0] && blockNumber < stageMask[1] {
		multiple = 32
	} else if blockNumber >= stageMask[1] && blockNumber < stageMask[2] {
		multiple = 16
	} else if blockNumber >= stageMask[2] && blockNumber < stageMask[3] {
		multiple = 8
	} else if blockNumber >= stageMask[3] && blockNumber < stageMask[4] {
		multiple = 4
	} else if blockNumber >= stageMask[4] && blockNumber < stageMask[5] {
		multiple = 2
	} else if blockNumber >= stageMask[5] {
		multiple = 1
	}

	blockReward := new(big.Int).Mul(blockRewardBase, big.NewInt(multiple))
	log.Trace("award master ", "coinbase:", header.Coinbase(), " number:", header.NumberU64(), "amount", blockReward)
	//reward to master
	state.AddBalance(header.Coinbase(), blockReward)

	//calc reward for shard blocks
	shardEnabled := header.ToHeader().ShardEnabled()
	shardEnabled[0] = shardEnabled[0] | 0x01
	shardsCount := 0
	for _, enabled := range shardEnabled {
		for i := 0; i < 8; i++ {
			if (enabled & bitMask[i]) != 0 {
				shardsCount++
			}
		}
	}
	rewardOfShard := uint32(10000/shardsCount) * uint32(multiple)

	rewardInHeader := []types.ShardState{}
	if parent != nil {
		rewardInHeader = parent.ToHeader().ShardState()
	} //rawDb.ReadRewardRemains

	rewardRemains := make(map[uint16]*types.ShardState)
	for _, shardState := range rewardInHeader {
		rewardRemains[shardState.ShardId] = &shardState
	}
	for seg, enabled := range shardEnabled {
		for i := 0; i < 8; i++ {
			if (enabled & bitMask[i]) != 0 {
				index := uint16(seg*8 + i)
				_, ok := rewardRemains[index]
				if !ok {
					rewardRemains[index] = &types.ShardState{index, 0, uint32(rewardOfShard)}
				} else {
					rewardRemains[index].RewardRemains = rewardRemains[index].RewardRemains + rewardOfShard
				}

			}
		}
	}
	blkInfos := make(map[uint16]types.ShardBlockInfos)
	for _, blk := range blks {
		blkInfos[blk.ShardId] = append(blkInfos[blk.ShardId], blk)
	}
	result := make([]types.ShardState, 0, shardsCount)
	for shardId, blkArray := range blkInfos {
		blockNo := uint64(0)
		remains := uint32(0)
		if ss, ok := rewardRemains[shardId]; ok {
			if ss.RewardRemains < uint32(len(blkArray))*rewardOfShard {
				rewardOfShard = ss.RewardRemains / uint32(len(blkArray))
			}
			remains = ss.RewardRemains
		}

		for _, oneBlock := range blkArray {
			if oneBlock.BlockNumber > blockNo {
				blockNo = oneBlock.BlockNumber
			}
			log.Trace("award shard ", "coinbase:", oneBlock.Coinbase, " number:", oneBlock.BlockNumber, "amount", rewardOfShard)
			//reward to master
			if remains > rewardOfShard {
				remains -= rewardOfShard
				state.AddBalance(oneBlock.Coinbase, new(big.Int).Mul(rewardBaseUint, big.NewInt(int64(rewardOfShard))))
			} else {
				state.AddBalance(oneBlock.Coinbase, new(big.Int).Mul(rewardBaseUint, big.NewInt(int64(remains))))
				remains = 0
			}

		}
		log.Trace(" check Remain:", "rewards:", len(rewardRemains), " with shardId:", shardId)
		if rewardRemains[shardId] == nil {
			fmt.Println(" error check Remain:", len(rewardRemains), " with shardId:", shardId)
		} else {
			rewardRemains[shardId].RewardRemains = remains
		}

		//update shard state

	}

	for shardId, remain := range rewardRemains {
		result = append(result, types.ShardState{shardId, remain.BlockNumber, remain.RewardRemains})
	}
	header.ToHeader().SetShardState(result)

}
