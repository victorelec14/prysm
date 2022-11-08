package eth1

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/pkg/errors"
	"github.com/prysmaticlabs/prysm/v3/config/params"
	contracts "github.com/prysmaticlabs/prysm/v3/contracts/deposit"
	"github.com/prysmaticlabs/prysm/v3/encoding/bytesutil"
	e2e "github.com/prysmaticlabs/prysm/v3/testing/endtoend/params"
	"github.com/prysmaticlabs/prysm/v3/testing/endtoend/types"
	"github.com/prysmaticlabs/prysm/v3/testing/util"
)

const depositGasLimit = 4000000

type Depositor struct {
	types.EmptyComponent
	Key       *keystore.Key
	Client    *ethclient.Client
	ChainID   *big.Int
	NetworkId *big.Int
}

var _ types.ComponentRunner = &Depositor{}

func (d *Depositor) SendAndMine(ctx context.Context, validatorNum, offset int, partial bool) error {
	txOps, err := bind.NewKeyedTransactorWithChainID(d.Key.PrivateKey, d.NetworkId)
	if err != nil {
		return err
	}
	txOps.Context = ctx
	txOps.GasLimit = depositGasLimit
	nonce, err := d.Client.PendingNonceAt(ctx, txOps.From)
	if err != nil {
		return err
	}
	txOps.Nonce = big.NewInt(0).SetUint64(nonce)

	contract, err := contracts.NewDepositContract(e2e.TestParams.ContractAddress, d.Client)
	if err != nil {
		return err
	}

	balances := make([]uint64, validatorNum+offset)
	for i := 0; i < len(balances); i++ {
		if i < len(balances)/2 && partial {
			balances[i] = params.BeaconConfig().MaxEffectiveBalance / 2
		} else {
			balances[i] = params.BeaconConfig().MaxEffectiveBalance
		}
	}
	deposits, trie, err := util.DepositsWithBalance(balances)
	if err != nil {
		return err
	}
	allDeposits := deposits
	allRoots := trie.Items()
	allBalances := balances
	if partial {
		deposits2, trie2, err := util.DepositsWithBalance(balances)
		if err != nil {
			return err
		}
		allDeposits = append(deposits, deposits2[:len(balances)/2]...)
		allRoots = append(trie.Items(), trie2.Items()[:len(balances)/2]...)
		allBalances = append(balances, balances[:len(balances)/2]...)
	}
	for index, dd := range allDeposits {
		if index < offset {
			continue
		}
		depositInGwei := big.NewInt(int64(allBalances[index]))
		txOps.Value = depositInGwei.Mul(depositInGwei, big.NewInt(int64(params.BeaconConfig().GweiPerEth)))
		_, err = contract.Deposit(txOps, dd.Data.PublicKey, dd.Data.WithdrawalCredentials, dd.Data.Signature, bytesutil.ToBytes32(allRoots[index]))
		if err != nil {
			return errors.Wrap(err, "unable to send transaction to contract")
		}
		txOps.Nonce = txOps.Nonce.Add(txOps.Nonce, big.NewInt(1))
	}

	// This is the "AndMine" part of the function. WaitForBlocks will spam transactions to/from the given key
	// to advance the EL chain and until the chain has advanced the requested amount.
	if err = WaitForBlocks(d.Client, d.Key, params.BeaconConfig().Eth1FollowDistance); err != nil {
		return fmt.Errorf("failed to mine blocks %w", err)
	}
	return nil
}
