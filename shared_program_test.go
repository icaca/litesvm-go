package litesvm

import (
	"bytes"
	"encoding/binary"
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
				if i%2 == 0 {
					require.NoError(t, s.AddProgramImmutable(id, elf.Defaults[3].Elf))
				} else {
					require.NoError(t, s.AddProgram(id, elf.Defaults[3].Elf))
				}
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

func TestImmutableProgramSourceInvalidation(t *testing.T) {
	s, other := mustNew(t), mustNew(t)
	id := solana.PublicKey{180}
	require.ErrorIs(t, s.AddProgramImmutable(id, []byte("invalid ELF")), ErrLiteSVM)
	source := bytes.Clone(elf.Defaults[3].Elf)
	require.NoError(t, s.AddProgramImmutable(id, source))
	dataKey, _, err := solana.FindProgramAddress([][]byte{id[:]}, solana.BPFLoaderUpgradeableProgramID)
	require.NoError(t, err)
	require.NoError(t, other.AddProgramImmutable(id, source))
	original := other.GetAccount(dataKey)
	require.Same(t, &s.mem.Map[dataKey].Data[0], &other.mem.Map[dataKey].Data[0])
	payer := solana.NewWallet()
	require.NoError(t, s.Airdrop(payer.PublicKey(), lamportsPerSol))
	run := func(payload string) *TxOutcome {
		out, err := s.SendLegacyTransaction(memoTx(t, s, payer.PrivateKey, id, []byte(payload)))
		require.NoError(t, err)
		return out
	}
	require.True(t, run("immutable source").IsOk())
	data := s.GetAccount(dataKey)
	data.Data[45] = 0
	require.Equal(t, original, s.GetAccount(dataKey), "GetAccount must return a private copy")
	require.NoError(t, s.SetAccount(dataKey, data))
	require.False(t, run("invalidated source").IsOk(), "a private account write must invalidate the VM binding")
	require.Equal(t, original, other.GetAccount(dataKey))
	require.Equal(t, elf.Defaults[3].Elf, source, "VM writes must not change the promised source")
	require.NoError(t, s.AddProgramImmutable(id, source))
	require.True(t, run("reinstalled source").IsOk(), "a warm immutable install must restore the original program")
	require.Same(t, &s.mem.Map[dataKey].Data[0], &other.mem.Map[dataKey].Data[0])
}

func TestSharedProgramDataTransactionExposure(t *testing.T) {
	s, other := mustNew(t), mustNew(t)
	id := solana.PublicKey{181}
	source := bytes.Clone(elf.Defaults[3].Elf)
	dataKey, _, err := solana.FindProgramAddress([][]byte{id[:]}, solana.BPFLoaderUpgradeableProgramID)
	require.NoError(t, err)
	require.NoError(t, other.AddProgramImmutable(id, source))
	original := other.GetAccount(dataKey)
	payer := solana.NewWallet()
	require.NoError(t, s.Airdrop(payer.PublicKey(), lamportsPerSol))
	table := solana.PublicKey{182}
	require.NoError(t, s.SetAccount(table, &solana.Account{
		Lamports: lamportsPerSol, Owner: solana.AddressLookupTableProgramID,
		Data: altAccountData(payer.PublicKey(), dataKey),
	}))
	require.NoError(t, s.WarpToSlot(1))

	for _, tc := range []struct {
		name                       string
		writable, simulate, lookup bool
	}{
		{name: "readonly simulation", simulate: true},
		{name: "writable failed transaction", writable: true},
		{name: "readonly lookup", lookup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, s.AddProgramImmutable(id, source))
			require.Same(t, &s.mem.Map[dataKey].Data[0], &other.mem.Map[dataKey].Data[0])
			meta := solana.Meta(dataKey)
			if tc.writable {
				meta.WRITE()
			}
			ix := solana.NewInstruction(id, solana.AccountMetaSlice{meta}, []byte(tc.name))
			blockhash, err := s.LatestBlockhash()
			require.NoError(t, err)
			opts := []solana.TransactionOption{solana.TransactionPayer(payer.PublicKey())}
			if tc.lookup {
				opts = append(opts, solana.TransactionAddressTables(map[solana.PublicKey]solana.PublicKeySlice{table: {dataKey}}))
			}
			tx, err := solana.NewTransaction([]solana.Instruction{ix}, blockhash, opts...)
			require.NoError(t, err)
			if tc.lookup {
				require.NotContains(t, tx.Message.AccountKeys, dataKey)
			}
			_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
				if key == payer.PublicKey() {
					return &payer.PrivateKey
				}
				return nil
			})
			require.NoError(t, err)
			encoded, err := tx.MarshalBinary()
			require.NoError(t, err)
			var out *TxOutcome
			if tc.simulate {
				out, err = s.SimulateVersionedTransaction(encoded)
			} else {
				out, err = s.SendVersionedTransaction(encoded)
			}
			require.NoError(t, err)
			require.False(t, out.IsOk(), "Memo requires referenced accounts to sign")
			require.NotSame(t, &s.mem.Map[dataKey].Data[0], &other.mem.Map[dataKey].Data[0])
			// Model a native write through an exposed alias, bypassing setters.
			s.mem.Map[dataKey].Data[45] = 0
			require.Equal(t, original, other.GetAccount(dataKey))
			require.NoError(t, s.AddProgramImmutable(id, source))
			require.Equal(t, original, s.GetAccount(dataKey))
		})
	}
}

func TestSharedProgramDataUpgradeAndClose(t *testing.T) {
	s, other := mustNew(t), mustNew(t)
	id := solana.PublicKey{183}
	source := bytes.Clone(elf.Defaults[3].Elf)
	require.NoError(t, s.AddProgramImmutable(id, source))
	require.NoError(t, other.AddProgramImmutable(id, source))
	dataKey, _, err := solana.FindProgramAddress([][]byte{id[:]}, solana.BPFLoaderUpgradeableProgramID)
	require.NoError(t, err)
	original := other.GetAccount(dataKey)
	payer := solana.NewWallet()
	require.NoError(t, s.Airdrop(payer.PublicKey(), lamportsPerSol))
	// Installed programs use the zero authority; disable crypto verification
	// while retaining the loader signer/authority checks.
	require.NoError(t, s.SetSigverify(false))
	require.NoError(t, s.WarpToSlot(1))
	runLoader := func(kind byte, metas solana.AccountMetaSlice) {
		ix := solana.NewInstruction(solana.BPFLoaderUpgradeableProgramID, metas, []byte{kind, 0, 0, 0})
		blockhash, err := s.LatestBlockhash()
		require.NoError(t, err)
		tx, err := solana.NewTransaction([]solana.Instruction{ix}, blockhash, solana.TransactionPayer(payer.PublicKey()))
		require.NoError(t, err)
		tx.Signatures = make([]solana.Signature, tx.Message.Header.NumRequiredSignatures)
		encoded, err := tx.MarshalBinary()
		require.NoError(t, err)
		out, err := s.SendLegacyTransaction(encoded)
		require.NoError(t, err)
		require.True(t, out.IsOk(), "%s", out.Error())
	}
	buffer := solana.PublicKey{184}
	replacement := elf.Defaults[3].Elf
	require.LessOrEqual(t, len(replacement), len(source))
	bufferData := make([]byte, 37+len(replacement))
	binary.LittleEndian.PutUint32(bufferData, 1)
	bufferData[4] = 1 // Some(zero authority)
	copy(bufferData[37:], replacement)
	require.NoError(t, s.SetAccount(buffer, &solana.Account{
		Lamports: lamportsPerSol, Owner: solana.BPFLoaderUpgradeableProgramID, Data: bufferData,
	}))
	runLoader(3, solana.AccountMetaSlice{
		solana.Meta(dataKey).WRITE(), solana.Meta(id).WRITE(), solana.Meta(buffer).WRITE(),
		solana.Meta(payer.PublicKey()).WRITE(), solana.Meta(solana.SysVarRentPubkey),
		solana.Meta(solana.SysVarClockPubkey), solana.Meta(solana.PublicKey{}).SIGNER(),
	})
	upgraded := s.GetAccount(dataKey)
	require.Equal(t, uint64(1), binary.LittleEndian.Uint64(upgraded.Data[4:12]))
	require.Equal(t, replacement, upgraded.Data[45:45+len(replacement)])
	require.Equal(t, original, other.GetAccount(dataKey))

	// Reinstall shared storage before exercising the close path independently.
	require.NoError(t, s.AddProgramImmutable(id, source))
	runLoader(5, solana.AccountMetaSlice{
		solana.Meta(dataKey).WRITE(), solana.Meta(payer.PublicKey()).WRITE(),
		solana.Meta(solana.PublicKey{}).SIGNER(), solana.Meta(id).WRITE(),
	})
	require.Nil(t, s.GetAccount(dataKey))
	require.Equal(t, original, other.GetAccount(dataKey))
	require.NoError(t, s.AddProgramImmutable(id, source))
	require.Equal(t, original, s.GetAccount(dataKey))
	require.Equal(t, elf.Defaults[3].Elf, source)
}
