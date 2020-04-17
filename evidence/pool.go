package evidence

import (
	"fmt"
	"sync"
	"time"

	dbm "github.com/tendermint/tm-db"

	clist "github.com/tendermint/tendermint/libs/clist"
	"github.com/tendermint/tendermint/libs/log"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/store"
	"github.com/tendermint/tendermint/types"
)

// Pool maintains a pool of valid evidence in an Store.
type Pool struct {
	logger log.Logger

	store        *Store
	evidenceList *clist.CList // concurrent linked-list of evidence

	// needed to load validators to verify evidence
	stateDB    dbm.DB
	blockStore *store.BlockStore

	// a map of active validators and respective last heights validator is active
	// if it was in validator set after EvidenceParams.MaxAgeNumBlocks or
	// currently is (ie. [MaxAgeNumBlocks, CurrentHeight])
	// In simple words, it means it's still bonded -> therefore slashable.
	valToLastHeight valToLastHeightMap

	// latest state
	mtx   sync.Mutex
	state sm.State
}

// Validator.Address -> Last height it was in validator set
type valToLastHeightMap map[string]int64

func NewPool(stateDB, evidenceDB dbm.DB, blockStore *store.BlockStore) *Pool {
	var (
		evidenceStore = NewStore(evidenceDB)
		state         = sm.LoadState(stateDB)
	)

	pool := &Pool{
		stateDB:         stateDB,
		blockStore:      blockStore,
		state:           state,
		logger:          log.NewNopLogger(),
		store:           evidenceStore,
		evidenceList:    clist.New(),
		valToLastHeight: buildValToLastHeightMap(state, stateDB),
	}

	// if pending evidence already in db, in event of prior failure, then load it to the evidenceList
	evList := evidenceStore.listEvidence(baseKeyPending, -1)
	for _, ev := range evList {
		// check evidence hasn't expired
		if pool.IsExpired(ev) {
			key := keyPending(ev)
			pool.store.db.Delete(key)
			continue
		}
		pool.evidenceList.PushBack(ev)
	}

	return pool
}

func (evpool *Pool) EvidenceFront() *clist.CElement {
	return evpool.evidenceList.Front()
}

func (evpool *Pool) EvidenceWaitChan() <-chan struct{} {
	return evpool.evidenceList.WaitChan()
}

// SetLogger sets the Logger.
func (evpool *Pool) SetLogger(l log.Logger) {
	evpool.logger = l
}

// PendingEvidence returns up to maxNum uncommitted evidence.
// If maxNum is -1, all evidence is returned. Pending evidence is in order of priority
func (evpool *Pool) PendingEvidence(maxNum int64) []types.Evidence {
	return evpool.store.PendingEvidence(maxNum)
}

// State returns the current state of the evpool.
func (evpool *Pool) State() sm.State {
	evpool.mtx.Lock()
	defer evpool.mtx.Unlock()
	return evpool.state
}

// Update loads the latest
func (evpool *Pool) Update(block *types.Block, state sm.State) {
	// sanity check
	if state.LastBlockHeight != block.Height {
		panic(
			fmt.Sprintf("Failed EvidencePool.Update sanity check: got state.Height=%d with block.Height=%d",
				state.LastBlockHeight,
				block.Height,
			),
		)
	}

	// update the state
	evpool.mtx.Lock()
	evpool.state = state
	evpool.mtx.Unlock()

	// remove evidence from pending and mark committed
	evpool.MarkEvidenceAsCommitted(block.Height, block.Time, block.Evidence.Evidence)

	evpool.cleanupValToLastHeight(block.Height)
}

// AddEvidence checks the evidence is valid and adds it to the pool. If
// evidence is composite (ConflictingHeadersEvidence), it will be broken up
// into smaller pieces.
func (evpool *Pool) AddEvidence(evidence types.Evidence) error {
	var (
		state  = evpool.State()
		evList = []types.Evidence{evidence}
	)

	valSet, err := sm.LoadValidators(evpool.stateDB, evidence.Height())
	if err != nil {
		return fmt.Errorf("can't load validators at height #%d: %w", evidence.Height(), err)
	}

	// Break composite evidence into smaller pieces.
	if ce, ok := evidence.(types.CompositeEvidence); ok {
		evpool.logger.Info("Breaking up composite evidence", "ev", evidence)

		blockMeta := evpool.blockStore.LoadBlockMeta(evidence.Height())
		if blockMeta == nil {
			return fmt.Errorf("don't have block meta at height #%d", evidence.Height())
		}

		if err := ce.VerifyComposite(&blockMeta.Header, valSet); err != nil {
			return err
		}

		evList = ce.Split(&blockMeta.Header, valSet, evpool.valToLastHeight)
	}

	for _, ev := range evList {
		ok, err := evpool.store.Has(evidence)
		if err != nil {
			return ErrDatabase{err}
		}
		if ok {
			return ErrEvidenceAlreadyStored{}
		}

		// For lunatic validator evidence, a header needs to be fetched.
		var header *types.Header
		if _, ok := ev.(*types.LunaticValidatorEvidence); ok {
			blockMeta := evpool.blockStore.LoadBlockMeta(ev.Height())
			if blockMeta == nil {
				return fmt.Errorf("don't have block meta at height #%d", ev.Height())
			}
			header = &blockMeta.Header
		}

		// 1) Verify against state.
		if err := sm.VerifyEvidence(evpool.stateDB, state, ev, header); err != nil {
			return fmt.Errorf("failed to verify %v: %w", ev, err)
		}

		// 2) Compute priority.
		_, val := valSet.GetByAddress(ev.Address())
		priority := val.VotingPower

		// 3) Save to store.
		err = evpool.store.addEvidence(ev, priority)
		if err != nil {
			return ErrDatabase{err}
		}

		// 4) Add evidence to clist.
		evpool.evidenceList.PushBack(ev)

		evpool.logger.Info("Verified new evidence of byzantine behaviour", "evidence", ev)
	}

	return nil
}

// MarkEvidenceAsCommitted marks all the evidence as committed and removes it
// from the queue.
func (evpool *Pool) MarkEvidenceAsCommitted(height int64, lastBlockTime time.Time, evidence []types.Evidence) {
	// make a map of committed evidence to remove from the clist
	blockEvidenceMap := make(map[string]struct{})
	for _, ev := range evidence {
		evpool.store.MarkEvidenceAsCommitted(ev)
		blockEvidenceMap[evMapKey(ev)] = struct{}{}
	}

	// remove committed evidence from the clist
	evidenceParams := evpool.State().ConsensusParams.Evidence
	evpool.removeEvidence(height, lastBlockTime, evidenceParams, blockEvidenceMap)
}

func (evpool *Pool) removeEvidence(
	height int64,
	lastBlockTime time.Time,
	params types.EvidenceParams,
	blockEvidenceMap map[string]struct{}) {

	for e := evpool.evidenceList.Front(); e != nil; e = e.Next() {
		var (
			ev           = e.Value.(types.Evidence)
			ageDuration  = lastBlockTime.Sub(ev.Time())
			ageNumBlocks = height - ev.Height()
		)

		// Remove the evidence if it's already in a block or if it's now too old.
		if _, ok := blockEvidenceMap[evMapKey(ev)]; ok ||
			(ageDuration > params.MaxAgeDuration && ageNumBlocks > params.MaxAgeNumBlocks) {
			// remove from clist
			evpool.evidenceList.Remove(e)
			e.DetachPrev()
		}
	}
}

func (evpool *Pool) cleanupValToLastHeight(blockHeight int64) {
	removeHeight := blockHeight - evpool.State().ConsensusParams.Evidence.MaxAgeNumBlocks
	if removeHeight >= 1 {
		valSet, err := sm.LoadValidators(evpool.stateDB, removeHeight)
		if err != nil {
			for _, val := range valSet.Validators {
				h, ok := evpool.valToLastHeight[string(val.Address)]
				if ok && h == removeHeight {
					delete(evpool.valToLastHeight, string(val.Address))
				}
			}
		}
	}
}

func (evpool *Pool) IsExpired(evidence types.Evidence) bool {
	var (
		params       = evpool.State().ConsensusParams.Evidence
		ageDuration  = evpool.State().LastBlockTime.Sub(evidence.Time())
		ageNumBlocks = evpool.State().LastBlockHeight - evidence.Height()
	)
	return ageNumBlocks > params.MaxAgeNumBlocks &&
		ageDuration > params.MaxAgeDuration
}

func evMapKey(ev types.Evidence) string {
	return string(ev.Hash())
}

func buildValToLastHeightMap(state sm.State, stateDB dbm.DB) valToLastHeightMap {
	var (
		valToLastHeight = make(map[string]int64)
		params          = state.ConsensusParams.Evidence

		numBlocks = int64(0)
		height    = state.LastBlockHeight
	)

	// From last height down to MaxAgeNumBlocks, put all validators into a map.
	for height >= 1 && numBlocks <= params.MaxAgeNumBlocks {
		valSet, err := sm.LoadValidators(stateDB, height)

		// last stored height -> return
		if _, ok := err.(sm.ErrNoValSetForHeight); ok {
			return valToLastHeight
		}

		for _, val := range valSet.Validators {
			key := string(val.Address)
			if _, ok := valToLastHeight[key]; !ok {
				valToLastHeight[key] = height
			}
		}

		height--
		numBlocks++
	}

	return valToLastHeight
}
