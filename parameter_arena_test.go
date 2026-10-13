package litesvm

import (
	"bytes"
	"sync"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/sonicfromnewyoke/mithril/pkg/features"
	"github.com/stretchr/testify/require"

	"github.com/LiteSVM/litesvm-go/internal/elf"
)

func TestParameterArenaTransactionIsolation(t *testing.T) {
	var workers sync.WaitGroup
	for range 2 {
		svm := mustNew(t)
		t.Cleanup(svm.Close)
		// Exercise the serialization path that leaves account data in the input buffer.
		svm.feats.DisableFeature(features.AccountDataDirectMapping)
		payer := solana.NewWallet()
		require.NoError(t, svm.Airdrop(payer.PublicKey(), 2*lamportsPerSol))

		workers.Add(1)
		go func() {
			defer workers.Done()
			mint := solana.NewWallet()
			rent, err := svm.MinimumBalanceForRentExemption(splMintLen)
			require.NoError(t, err)
			initMint, err := token.NewInitializeMint2InstructionBuilder().
				SetDecimals(6).SetMintAuthority(payer.PublicKey()).
				SetMintAccount(mint.PublicKey()).ValidateAndBuild()
			require.NoError(t, err)
			outcome := signAndSend(t, svm, payer.PrivateKey, []solana.Instruction{
				system.NewCreateAccountInstruction(rent, splMintLen, solana.TokenProgramID,
					payer.PublicKey(), mint.PublicKey()).Build(),
				initMint,
			}, mint.PrivateKey)
			requireTxOK(t, outcome, "initialize mint")
			want := svm.GetAccount(mint.PublicKey())
			require.NotNil(t, want)
			require.Equal(t, byte(1), want.Data[45])
			previous := outcome.ExecutionAccounts()
			var savedMint []byte
			for _, acct := range previous {
				if acct.Address == mint.PublicKey() {
					savedMint = bytes.Clone(acct.Account.Data)
				}
			}
			require.NotEmpty(t, savedMint)

			blockhash, err := svm.LatestBlockhash()
			require.NoError(t, err)
			for _, input := range [][]byte{bytes.Repeat([]byte("a"), 33), {0xff}, []byte("ok")} {
				ix := solana.NewInstruction(elf.Defaults[3].ProgramID,
					solana.AccountMetaSlice{solana.Meta(payer.PublicKey()).SIGNER()}, input)
				tx, err := solana.NewTransaction([]solana.Instruction{ix}, blockhash,
					solana.TransactionPayer(payer.PublicKey()))
				require.NoError(t, err)
				_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey { return &payer.PrivateKey })
				require.NoError(t, err)
				raw, err := tx.MarshalBinary()
				require.NoError(t, err)
				simulated, err := svm.SimulateLegacyTransaction(raw)
				require.NoError(t, err)
				sent, err := svm.SendLegacyTransaction(raw)
				require.NoError(t, err)
				require.Equal(t, input[0] != 0xff, sent.IsOk(), "%s", sent.Error())
				require.Equal(t, simulated.IsOk(), sent.IsOk())
				require.Equal(t, simulated.Logs(), sent.Logs())
				require.Equal(t, want, svm.GetAccount(mint.PublicKey()), "later transactions must not alter the mint")
				for _, acct := range previous {
					if acct.Address == mint.PublicKey() {
						require.Equal(t, savedMint, acct.Account.Data, "old outcomes must stay detached")
					}
				}
			}
		}()
	}
	workers.Wait()
}
