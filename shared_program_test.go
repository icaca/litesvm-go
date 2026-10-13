package litesvm

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/LiteSVM/litesvm-go/internal/elf"
	"github.com/gagliardetto/solana-go"
	"github.com/sonicfromnewyoke/mithril/pkg/sbpf"
	"github.com/stretchr/testify/require"
)

func TestSharedProgramConcurrentVMs(t *testing.T) {
	const workers = 8
	programs := make(chan *sbpf.Program, workers)
	t.Run("VMs", func(t *testing.T) {
		for i := range workers {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				s := mustNew(t)
				id := solana.PublicKey{178}
				require.NoError(t, s.AddProgram(id, elf.Defaults[3].Elf))
				dataKey, _, err := solana.FindProgramAddress([][]byte{id[:]}, solana.BPFLoaderUpgradeableProgramID)
				require.NoError(t, err)
				entry, ok := s.stubDb.MaybeGetProgramFromCache(dataKey)
				require.True(t, ok)
				before := *entry.Program
				before.RO = bytes.Clone(before.RO)
				before.TextBytes = bytes.Clone(before.TextBytes)
				before.Text = slices.Clone(before.Text)
				before.Funcs = maps.Clone(before.Funcs)
				before.CallTargets = slices.Clone(before.CallTargets)
				before.ImportedSyscalls = slices.Clone(before.ImportedSyscalls)
				payer := solana.NewWallet()
				require.NoError(t, s.Airdrop(payer.PublicKey(), lamportsPerSol))
				out, err := s.SendLegacyTransaction(memoTx(t, s, payer.PrivateKey, id, []byte("parallel shared program")))
				require.NoError(t, err)
				require.True(t, out.IsOk(), "%s", out.Error())
				require.Equal(t, before, *entry.Program, "execution changed shared program storage")
				programs <- entry.Program
			})
		}
	})
	close(programs)
	var shared *sbpf.Program
	for p := range programs {
		if shared == nil {
			shared = p
		}
		require.Same(t, shared, p)
	}
}

func TestPrivateProgramSourceInvalidation(t *testing.T) {
	s := mustNew(t)
	payer := solana.NewWallet()
	require.NoError(t, s.Airdrop(payer.PublicKey(), lamportsPerSol))
	id := solana.PublicKey{179}
	source := bytes.Clone(elf.Defaults[3].Elf)
	require.NoError(t, s.AddProgram(id, source))
	dataKey, _, err := solana.FindProgramAddress([][]byte{id[:]}, solana.BPFLoaderUpgradeableProgramID)
	require.NoError(t, err)
	run := func(payload string) *TxOutcome {
		out, err := s.SendLegacyTransaction(memoTx(t, s, payer.PrivateKey, id, []byte(payload)))
		require.NoError(t, err)
		return out
	}
	source[0] = 0 // AddProgram must own its bytes.
	require.True(t, run("private source").IsOk())
	data := s.GetAccount(dataKey)
	data.Data[45] = 0
	require.True(t, run("GetAccount copy").IsOk())
	require.NoError(t, s.SetAccount(dataKey, data))
	require.False(t, run("same slot invalid ELF").IsOk())
	require.NoError(t, s.AddProgram(id, elf.Defaults[3].Elf))
	require.True(t, run("reinstalled").IsOk())

	// Even a readonly alias in simulation revokes trust before native code runs.
	ix := solana.NewInstruction(id, solana.AccountMetaSlice{solana.Meta(dataKey)}, []byte("readonly alias"))
	blockhash, err := s.LatestBlockhash()
	require.NoError(t, err)
	tx, err := solana.NewTransaction([]solana.Instruction{ix}, blockhash, solana.TransactionPayer(payer.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer.PublicKey() {
			return &payer.PrivateKey
		}
		return nil
	})
	require.NoError(t, err)
	encoded, err := tx.MarshalBinary()
	require.NoError(t, err)
	_, err = s.SimulateLegacyTransaction(encoded)
	require.NoError(t, err)
	_, trusted := s.immutablePrograms[dataKey]
	require.False(t, trusted)
	// Model a native write through the transaction alias, bypassing setters.
	s.mem.Map[dataKey].Data[45] = 0
	require.False(t, run("alias mutation").IsOk())
	require.NoError(t, s.AddProgram(id, elf.Defaults[3].Elf))
	closed := s.GetAccount(dataKey)
	closed.Lamports = 0
	require.NoError(t, s.SetAccount(dataKey, closed))
	require.False(t, run("closed source").IsOk())
	require.NoError(t, s.AddProgram(id, elf.Defaults[3].Elf))
	require.True(t, run("after close and reinstall").IsOk())
}
