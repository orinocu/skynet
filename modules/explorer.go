package modules

import (
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/types"
)

// ExplorerDir is the name of the directory that is typically used for the
// explorer.
const ExplorerDir = "explorer"

// HashTypes for various entries in the explorer.
const (
	BlockHashType           = "Blk"
	TransactionHashType     = "Tx"
	SiacoinOutputIdHashType = "Scoid"
	FileContractIdHashType  = "Fcid"
	SiafundOutputIdHashType = "Sfoid"
	UnlockHashType          = "Uh"
)

type (
	// BlockFacts returns a bunch of statistics about the consensus set as they
	// were at a specific block.
	BlockFacts struct {
		BlockID           types.BlockID     `json:"blockid"`
		Difficulty        types.Currency    `json:"difficulty"`
		EstimatedHashrate types.Currency    `json:"estimatedhashrate"`
		Height            types.BlockHeight `json:"height"`
		MaturityTimestamp types.Timestamp   `json:"maturitytimestamp"`
		Target            types.Target      `json:"target"`
		TotalCoins        types.Currency    `json:"totalcoins"`

		// Transaction type counts.
		MinerPayoutCount          uint64 `json:"minerpayoutcount"`
		TransactionCount          uint64 `json:"transactioncount"`
		SiacoinInputCount         uint64 `json:"siacoininputcount"`
		SiacoinOutputCount        uint64 `json:"siacoinoutputcount"`
		FileContractCount         uint64 `json:"filecontractcount"`
		FileContractRevisionCount uint64 `json:"filecontractrevisioncount"`
		StorageProofCount         uint64 `json:"storageproofcount"`
		SiafundInputCount         uint64 `json:"siafundinputcount"`
		SiafundOutputCount        uint64 `json:"siafundoutputcount"`
		MinerFeeCount             uint64 `json:"minerfeecount"`
		ArbitraryDataCount        uint64 `json:"arbitrarydatacount"`
		TransactionSignatureCount uint64 `json:"transactionsignaturecount"`

		// Factoids about file contracts.
		ActiveContractCost  types.Currency `json:"activecontractcost"`
		ActiveContractCount uint64         `json:"activecontractcount"`
		ActiveContractSize  types.Currency `json:"activecontractsize"`
		TotalContractCost   types.Currency `json:"totalcontractcost"`
		TotalContractSize   types.Currency `json:"totalcontractsize"`
		TotalRevisionVolume types.Currency `json:"totalrevisionvolume"`
	}

	// Explorer tracks the blockchain and provides tools for gathering
	// statistics and finding objects or patterns within the blockchain.
	Explorer interface {
		// Block returns the block that matches the input block id. The bool
		// indicates whether the block appears in the blockchain.
		Block(types.BlockID) (types.Block, types.BlockHeight, bool)

		// BlockFacts returns a set of statistics about the blockchain as they
		// appeared at a given block.
		BlockFacts(types.BlockHeight) (BlockFacts, bool)

		// LatestBlockFacts returns the block facts of the last block
		// in the explorer's database.
		LatestBlockFacts() BlockFacts

		// Transaction returns the block that contains the input transaction
		// id. The transaction itself is either the block (indicating the miner
		// payouts are somehow involved), or it is a transaction inside of the
		// block. The bool indicates whether the transaction is found in the
		// consensus set.
		Transaction(types.TransactionID) (types.Block, types.BlockHeight, bool)

		// UnlockHash returns all of the transaction ids associated with the
		// provided unlock hash.
		UnlockHash(types.UnlockHash) []types.TransactionID

		// SiacoinOutput will return the siacoin output associated with the
		// input id.
		SiacoinOutput(types.SiacoinOutputID) (types.SiacoinOutput, bool)

		// SiacoinOutputID returns all of the transaction ids associated with
		// the provided siacoin output id.
		SiacoinOutputID(types.SiacoinOutputID) []types.TransactionID

		// PendingTransactions gets the list of transactions currently in the mempool
		// These are pruned as transactions are confirmed or reverted, so this should
		// only be used as a snapshot
		PendingTransactions() []types.Transaction

		//HashType returns the type of a hash as a string representation.
		HashType(crypto.Hash) (string, error)

		// FileContractHistory returns the history associated with a file
		// contract, which includes the file contract itself and all of the
		// revisions that have been submitted to the blockchain. The first bool
		// indicates whether the file contract exists, and the second bool
		// indicates whether a storage proof was successfully submitted for the
		// file contract.
		FileContractHistory(types.FileContractID) (fc types.FileContract, fcrs []types.FileContractRevision, fcExists bool, storageProofExists bool)

		// FileContractID returns all of the transaction ids associated with
		// the provided file contract id.
		FileContractID(types.FileContractID) []types.TransactionID

		// SiafundOutput will return the siafund output associated with the
		// input id.
		SiafundOutput(types.SiafundOutputID) (types.SiafundOutput, bool)

		// SiafundOutputID returns all of the transaction ids associated with
		// the provided siafund output id.
		SiafundOutputID(types.SiafundOutputID) []types.TransactionID

		Close() error
	}
)
