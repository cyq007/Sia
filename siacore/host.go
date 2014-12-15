package siacore

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"

	"github.com/NebulousLabs/Andromeda/consensus"
	"github.com/NebulousLabs/Andromeda/encoding"
	"github.com/NebulousLabs/Andromeda/hash"
)

const (
	AcceptContractResponse = "accept"
	StorageProofReorgDepth = 6 // How many blocks to wait before submitting a storage proof.
)

// ContractEntry houses a single contract with its id - you cannot derive the
// id of a contract without having the transaction. Rather than keep the whole
// transaction, we store only the id.
type ContractEntry struct {
	ID       consensus.ContractID
	Contract *consensus.FileContract
}

// Host is the persistent structure handles storage requests from clients and
// manages the submission of storage proofs.
type Host struct {
	Settings HostAnnouncement

	SpaceRemaining int64

	Files map[hash.Hash]string
	Index int

	ForwardContracts  map[consensus.BlockHeight][]ContractEntry
	BackwardContracts map[consensus.BlockHeight][]ContractEntry

	sync.RWMutex
}

// CreateHost returns an initialized host.
func CreateHost() (h *Host) {
	return &Host{
		Files:             make(map[hash.Hash]string),
		ForwardContracts:  make(map[consensus.BlockHeight][]ContractEntry),
		BackwardContracts: make(map[consensus.BlockHeight][]ContractEntry),
	}
}

// HostSettings returns the host's settings.
func (e *Environment) HostSettings() HostAnnouncement {
	e.host.RLock()
	defer e.host.RUnlock()
	return e.host.Settings
}

// SetHostSettings changes the settings according to the input. Need a setter
// because Environment.host is not exported.
func (e *Environment) SetHostSettings(ha HostAnnouncement) {
	e.host.Lock()
	defer e.host.Unlock()

	e.host.SpaceRemaining += (ha.TotalStorage - e.host.Settings.TotalStorage)

	e.host.Settings = ha
}

// HostSpaceRemaining returns the amount of unsold space that the host has
// allocated.
func (e *Environment) HostSpaceRemaining() int64 {
	e.host.RLock()
	defer e.host.RUnlock()
	return e.host.SpaceRemaining
}

// Wallet.HostAnnounceSelf() creates a host announcement transaction, adding
// information to the arbitrary data and then signing the transaction.
func (e *Environment) HostAnnounceSelf(freezeVolume consensus.Currency, freezeUnlockHeight consensus.BlockHeight, minerFee consensus.Currency) (t consensus.Transaction, err error) {
	e.host.RLock()
	info := e.host.Settings
	e.host.RUnlock()

	// Fund the transaction.
	err = e.wallet.FundTransaction(freezeVolume+minerFee, &t)
	if err != nil {
		return
	}

	// Add the miner fee.
	t.MinerFees = append(t.MinerFees, minerFee)

	// Add the output with the freeze volume.
	freezeConditions := e.wallet.SpendConditions
	freezeConditions.TimeLock = freezeUnlockHeight
	t.Outputs = append(t.Outputs, consensus.Output{Value: freezeVolume, SpendHash: freezeConditions.CoinAddress()})
	info.FreezeIndex = uint64(len(t.Outputs) - 1)
	info.SpendConditions = freezeConditions

	// Frozen money can't currently be recovered.
	/*
		num, exists := w.OpenFreezeConditions[freezeUnlockHeight]
		if exists {
			w.OpenFreezeConditions[freezeUnlockHeight] = num + 1
		} else {
			w.OpenFreezeConditions[freezeUnlockHeight] = 1
		}
	*/

	// Add the announcement as arbitrary data.
	prefixBytes := encoding.Marshal(HostAnnouncementPrefix)
	announcementBytes := encoding.Marshal(info)
	t.ArbitraryData = append(prefixBytes, announcementBytes...)

	// Sign the transaction.
	for i := range t.Inputs {
		err = e.wallet.SignTransaction(&t, consensus.CoveredFields{WholeTransaction: true}, i)
		if err != nil {
			return
		}
	}

	// Give the transaction to the state.
	err = e.AcceptTransaction(t)
	if err != nil {
		return
	}

	return
}

// considerContract takes a contract and verifies that the negotiations, such
// as price, tolerance, etc. are all valid within the host settings. If so,
// inputs are added to fund the burn part of the contract fund, then the
// updated contract is signed and returned.
//
// TODO: Reconsider locking strategy for this function.
func (e *Environment) considerContract(t consensus.Transaction) (nt consensus.Transaction, err error) {
	// Set the new transaction equal to the old transaction. Pretty sure that
	// go does not allow you to return the same variable that was used as
	// input. We could use a pointer, but that might be a bad idea. This call
	// is happening over the network anyway.
	nt = t

	e.host.Lock()
	defer e.host.Unlock()

	contractDuration := nt.FileContracts[0].End - nt.FileContracts[0].Start // Duration according to the contract.
	fullDuration := nt.FileContracts[0].End - e.Height()                    // Duration that the host will actually be storing the file.
	fileSize := nt.FileContracts[0].FileSize

	// Check that there is only one file contract.
	if len(nt.FileContracts) != 1 {
		err = errors.New("transaction must have exactly one contract")
		return
	}

	// Check that the file size listed in the contract is in bounds.
	if fileSize < e.host.Settings.MinFilesize || fileSize > e.host.Settings.MaxFilesize {
		err = fmt.Errorf("file is of incorrect size - filesize %v, min %v, max %v", fileSize, e.host.Settings.MinFilesize, e.host.Settings.MaxFilesize)
		return
	}
	// Check that there is space for the file.
	if fileSize > uint64(e.host.SpaceRemaining) {
		err = errors.New("host is at capacity and can not take more files.")
		return
	}

	// Check that the duration of the contract is in bounds.
	if fullDuration < e.host.Settings.MinDuration || fullDuration > e.host.Settings.MaxDuration {
		err = errors.New("contract duration is out of bounds")
		return
	}

	// Check that challenges will not be happening too frequently or infrequently.
	if nt.FileContracts[0].ChallengeWindow < e.host.Settings.MinChallengeWindow || nt.FileContracts[0].ChallengeWindow > e.host.Settings.MaxChallengeWindow {
		err = errors.New("challenges frequency is too often")
		return
	}

	// Check that tolerance is acceptible.
	if nt.FileContracts[0].Tolerance < e.host.Settings.MinTolerance {
		err = errors.New("tolerance is too low")
		return
	}

	// Outputs for successful proofs need to go to the correct address.
	if nt.FileContracts[0].ValidProofAddress != e.host.Settings.CoinAddress {
		err = errors.New("coins are not paying out to correct address")
		return
	}

	// Outputs for successful proofs need to match the price.
	requiredSize := e.host.Settings.Price * consensus.Currency(fileSize) * consensus.Currency(nt.FileContracts[0].ChallengeWindow)
	if nt.FileContracts[0].ValidProofPayout < requiredSize {
		err = errors.New("valid proof payout is too low")
		return
	}

	// Output for failed proofs needs to be the 0 address.
	emptyAddress := consensus.CoinAddress{}
	if nt.FileContracts[0].MissedProofAddress != emptyAddress {
		err = errors.New("burn payout needs to go to the empty address")
		return
	}

	// Verify that output for failed proofs matches burn.
	maxBurn := e.host.Settings.Burn * consensus.Currency(fileSize) * consensus.Currency(nt.FileContracts[0].ChallengeWindow)
	if nt.FileContracts[0].MissedProofPayout > maxBurn {
		err = errors.New("burn payout is too high for a missed proof.")
		return
	}

	// Verify that the contract fund covers the payout and burn for the whole
	// duration.
	requiredFund := (e.host.Settings.Burn + e.host.Settings.Price) * consensus.Currency(fileSize) * consensus.Currency(contractDuration)
	if nt.FileContracts[0].ContractFund < requiredFund {
		err = errors.New("ContractFund does not cover the entire duration of the contract.")
		return
	}

	// Add some inputs and outputs to the transaction to fund the burn half.
	existingInputs := len(nt.Inputs)
	err = e.wallet.FundTransaction(e.host.Settings.Burn*consensus.Currency(fileSize)*consensus.Currency(contractDuration), &nt)
	if err != nil {
		fmt.Println(err)
		err = errors.New("Host is having trouble - sorry!")
		return
	}
	for i := existingInputs; i < len(nt.Inputs); i++ {
		err = e.wallet.SignTransaction(&nt, consensus.CoveredFields{WholeTransaction: true}, i)
		if err != nil {
			return
		}
	}

	// Check that the transaction is valid after the host signature.
	e.state.RLock()
	err = e.state.ValidTransaction(nt)
	e.state.RUnlock()
	if err != nil {
		fmt.Println(err)
		err = errors.New("post-verified transaction not valid - most likely a client error, but could be a host error too")
		return
	}

	return
}

// NegotiateContract is an RPC that negotiates a file contract. If the
// negotiation is successful, the file is downloaded and the host begins
// submitting proofs of storage.
//
// TODO: Reconsider locking model for this function.
func (e *Environment) NegotiateContract(conn net.Conn, data []byte) (err error) {
	// Read the transaction.
	var t consensus.Transaction
	if err = encoding.Unmarshal(data, &t); err != nil {
		return
	}

	// Check that the contained FileContract fits host criteria for taking
	// files.
	if t, err = e.considerContract(t); err != nil {
		_, err = encoding.WriteObject(conn, err.Error())
		return
	} else if _, err = encoding.WriteObject(conn, AcceptContractResponse); err != nil {
		return
	}

	// Create file.
	filename := e.hostDir + strconv.Itoa(e.host.Index)
	file, err := os.Create(filename)
	if err != nil {
		return
	}
	defer file.Close()
	// don't keep the file around if there's an error
	defer func() {
		if err != nil {
			os.Remove(filename)
		}
	}()

	// Download file contents
	_, err = io.CopyN(file, conn, int64(t.FileContracts[0].FileSize))
	if err != nil {
		return
	}

	// Check that the file matches the merkle root in the contract.
	_, err = file.Seek(0, 0)
	if err != nil {
		return
	}
	merkleRoot, err := hash.ReaderMerkleRoot(file, hash.CalculateSegments(t.FileContracts[0].FileSize))
	if err != nil {
		return
	}
	if merkleRoot != t.FileContracts[0].FileMerkleRoot {
		err = errors.New("uploaded file has wrong merkle root")
		return
	}

	// Check that the file arrived in time.
	if e.Height() >= t.FileContracts[0].Start-2 {
		err = errors.New("file not uploaded in time, refusing to go forward with contract")
		return
	}

	// record filename for later retrieval
	e.host.Lock()
	e.host.Files[t.FileContracts[0].FileMerkleRoot] = strconv.Itoa(e.host.Index)
	e.host.Index++
	e.host.Unlock()

	// Submit the transaction.
	err = e.AcceptTransaction(t)
	if err != nil {
		return
	}

	// Put the contract in a list where the host will be performing proofs of
	// storage.
	firstProof := t.FileContracts[0].Start + StorageProofReorgDepth
	e.host.ForwardContracts[firstProof] = append(e.host.ForwardContracts[firstProof], ContractEntry{ID: t.FileContractID(0), Contract: &t.FileContracts[0]})
	fmt.Println("Accepted contract")

	return
}

// RetrieveFile is an RPC that uploads a specified file to a client.
func (e *Environment) RetrieveFile(conn net.Conn, data []byte) (err error) {
	// Get the filename.
	var merkle hash.Hash
	if err = encoding.Unmarshal(data, &merkle); err != nil {
		return
	}

	// Verify the file exists.
	e.host.RLock()
	filename, exists := e.host.Files[merkle]
	e.host.RUnlock()
	if !exists {
		fmt.Println("RetrieveFile: no record of file with that hash")
		return errors.New("no record of that file")
	}

	// Open the file.
	file, err := os.Open(e.hostDir + filename)
	if err != nil {
		fmt.Println("RetrieveFile:", err)
		return
	}
	defer file.Close()

	// Transmit the file.
	_, err = io.Copy(conn, file)
	if err != nil {
		return
	}

	return
}

// Create a proof of storage for a contract, using the state height to
// determine the random seed. Create proof must be under a host and state lock.
func (e *Environment) createStorageProof(contractEntry ContractEntry, stateHeight consensus.BlockHeight) (sp consensus.StorageProof, err error) {
	// Get the file associated with the contract.
	filename, ok := e.host.Files[contractEntry.Contract.FileMerkleRoot]
	if !ok {
		err = errors.New("no record of that file")
	}

	// Open the file.
	file, err := os.Open(e.hostDir + filename)
	if err != nil {
		return
	}
	defer file.Close()

	// Build the proof using the hash library.
	numSegments := hash.CalculateSegments(contractEntry.Contract.FileSize)
	windowIndex, err := contractEntry.Contract.WindowIndex(stateHeight)
	if err != nil {
		return
	}
	segmentIndex, err := e.state.StorageProofSegmentIndex(contractEntry.ID, windowIndex)
	if err != nil {
		return
	}
	base, hashSet, err := hash.BuildReaderProof(file, numSegments, segmentIndex)
	if err != nil {
		return
	}
	sp = consensus.StorageProof{contractEntry.ID, windowIndex, base, hashSet}
	return
}

// storageProofMaintenance tracks when storage proofs need to be submitted as
// transactions, then creates the proof and submits the transaction.
// storageProofMaintenance must be under a state and host lock.
//
// TODO: Make sure that when a contract terminates, the space is returned to
// the unsold space pool, file is deleted, etc.
//
// TODO: Have some method for pruning the backwards contracts map.
//
// TODO: Make sure that hosts don't need to submit a storage proof for the last
// window.
func (e *Environment) storageProofMaintenance(initialStateHeight consensus.BlockHeight, rewoundBlocks []consensus.BlockID, appliedBlocks []consensus.BlockID) {
	// Resubmit any proofs that changed as a result of the rewinding.
	height := initialStateHeight
	var proofs []consensus.StorageProof
	for _ = range rewoundBlocks {
		needActionContracts := e.host.BackwardContracts[height]
		for _, contractEntry := range needActionContracts {
			proof, err := e.createStorageProof(contractEntry, height)
			if err != nil {
				fmt.Println("High Priority Error: storage proof failed:", err)
				continue
			}
			proofs = append(proofs, proof)
		}
		height--
	}

	// Submit any proofs that are triggered as the result of new blocks being added.
	for _ = range appliedBlocks {
		needActionContracts := e.host.ForwardContracts[height]
		for _, contractEntry := range needActionContracts {
			proof, err := e.createStorageProof(contractEntry, height)
			if err != nil {
				fmt.Println("High Priority Error: storage proof failed:", err)
				// TODO: Do something that will have the program try again, or
				// revitalize or whatever.
				continue
			}
			proofs = append(proofs, proof)

			// Add this contract proof to the backwards contracts list.
			e.host.BackwardContracts[height-StorageProofReorgDepth+1] = append(e.host.BackwardContracts[height-StorageProofReorgDepth+1], contractEntry)

			// Add this contract entry to ForwardContracts windowsize blocks
			// into the future if the contract has another window.
			nextProof := height + contractEntry.Contract.ChallengeWindow
			if nextProof < contractEntry.Contract.End {
				e.host.ForwardContracts[nextProof] = append(e.host.ForwardContracts[nextProof], contractEntry)
			} else {
				// Delete the file, etc. ==> Can't do this until we resolve the
				// collision problem.
			}
		}
		delete(e.host.ForwardContracts, height)
		height++
	}

	// Create the transaction that submits the storage proof.
	if len(proofs) != 0 {
		txn := consensus.Transaction{
			MinerFees:     []consensus.Currency{10},
			StorageProofs: proofs,
		}
		err := e.wallet.FundTransaction(10, &txn)
		if err != nil {
			fmt.Println("High Priority Error: FundTransaction failed during storageProofMaintenance:", err)
		}
		for i := range txn.Inputs {
			err = e.wallet.SignTransaction(&txn, consensus.CoveredFields{WholeTransaction: true}, i)
			if err != nil {
				fmt.Println("High Priority Error: SignTransaction failed during storageProofMaintenance:", err)
			}
		}
		err = e.AcceptTransaction(txn)
		if err != nil {
			fmt.Println("High Priority Error: accept transaction failed during storageProofMaintenance:", err)
		}
	}
}