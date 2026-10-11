package litesvm

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/compute-budget"
	"github.com/gagliardetto/solana-go/programs/system"
)

func TestDiagnosticExecution(t *testing.T) {
	svm, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer svm.Close()
	if err := svm.SetDiagnosticExecution(DiagnosticExecutionConfig{PreserveRentEpoch: true}); err == nil {
		t.Fatal("accepted rent preservation without diagnostic limit")
	}
	priv, payer, recipient, _ := buildSignedTransfer(t, svm, 1_000_000_000, 1_000_000)
	before := svm.GetAccount(payer)
	before.RentEpoch = 42
	if err := svm.SetAccount(payer, before); err != nil {
		t.Fatal(err)
	}
	bh, err := svm.LatestBlockhash()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := solana.NewTransaction([]solana.Instruction{
		computebudget.NewSetComputeUnitLimitInstruction(100_000).Build(),
		computebudget.NewSetComputeUnitPriceInstruction(50_000).Build(),
		system.NewTransferInstruction(1_000_000, payer, recipient).Build(),
		system.NewTransferInstruction(2_000_000_000, payer, recipient).Build(),
	}, bh, solana.TransactionPayer(payer))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Sign(func(k solana.PublicKey) *solana.PrivateKey {
		if k == payer {
			return &priv
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(wire)
	if err := svm.SetDiagnosticExecution(DiagnosticExecutionConfig{ComputeUnitLimit: 1}); err != nil {
		t.Fatal(err)
	}
	limited, err := svm.SimulateVersionedTransaction(wire)
	if err != nil {
		t.Fatal(err)
	}
	if limited.IsOk() || limited.ComputeUnits() != 1 || limited.Fee() != 10_000 {
		t.Fatalf("diagnostic meter or fee incorrect: CU=%d fee=%d error=%s", limited.ComputeUnits(), limited.Fee(), limited.Error())
	}
	if err := svm.SetDiagnosticExecution(DiagnosticExecutionConfig{ComputeUnitLimit: 2_000_000, PreserveRentEpoch: true}); err != nil {
		t.Fatal(err)
	}
	out, err := svm.SendVersionedTransaction(wire)
	if err != nil {
		t.Fatal(err)
	}
	if out.IsOk() || out.RuntimeError() == nil {
		t.Fatalf("expected runtime failure: %s", out.Error())
	}
	if out.Fee() != 10_000 {
		t.Fatalf("diagnostic limit changed transaction fee: %d, want 10000", out.Fee())
	}
	if !bytes.Equal(original, wire) {
		t.Fatal("mutated transaction wire")
	}
	invocations := out.InvocationCU()
	if len(invocations) != 4 || !invocations[0].Succeeded || !invocations[1].Succeeded || !invocations[2].Succeeded || invocations[3].Succeeded {
		t.Fatalf("invocations: %+v", invocations)
	}
	var total uint64
	for i, v := range invocations {
		program := solana.SystemProgramID
		if i < 2 {
			program = computebudget.ProgramID
		}
		if v.Program != program || v.Depth != 1 {
			t.Fatalf("invocation: %+v", v)
		}
		total += v.Units
	}
	if total != out.ComputeUnits() {
		t.Fatalf("invocation CU %d != total %d", total, out.ComputeUnits())
	}
	var snapshot *solana.Account
	for _, a := range out.ExecutionAccounts() {
		if a.Address == payer {
			snapshot = a.Account
		}
	}
	if snapshot == nil || snapshot.Lamports != before.Lamports-out.Fee()-1_000_000 || snapshot.RentEpoch != 42 {
		t.Fatalf("missing failed-execution snapshot: %+v", snapshot)
	}
	if got := svm.GetAccount(payer); got.Lamports != before.Lamports-out.Fee() {
		t.Fatalf("rollback failed: %+v", got)
	}
	if err := svm.SetDiagnosticExecution(DiagnosticExecutionConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := svm.Airdrop(payer, 1_000_000); err != nil {
		t.Fatal(err)
	}
	if snapshot.Lamports != before.Lamports-out.Fee()-1_000_000 {
		t.Fatal("snapshot aliases live state")
	}
}

func TestValidateProgramForExecution(t *testing.T) {
	f, err := NewFeatureSetAllEnabled()
	if err != nil {
		t.Fatal(err)
	}
	program, err := os.ReadFile(loggingProgramPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateProgramForExecution(program, f); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProgramForExecution([]byte("invalid ELF"), f); !errors.Is(err, ErrLiteSVM) {
		t.Fatalf("invalid ELF: %v", err)
	}
	if err := ValidateProgramForExecution(program, nil); !errors.Is(err, ErrLiteSVM) {
		t.Fatalf("nil feature set: %v", err)
	}
}
