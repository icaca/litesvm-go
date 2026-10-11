package litesvm

import (
	"fmt"

	"github.com/gagliardetto/solana-go"
	"github.com/sonicfromnewyoke/mithril/pkg/sbpf"
	"github.com/sonicfromnewyoke/mithril/pkg/sbpf/loader"
	"github.com/sonicfromnewyoke/mithril/pkg/sealevel"
)

// DiagnosticExecutionConfig overrides the execution meter without changing
// transaction instructions. A zero value restores normal execution.
// PreserveRentEpoch is available only with a nonzero diagnostic limit.
type DiagnosticExecutionConfig struct {
	ComputeUnitLimit  uint32
	PreserveRentEpoch bool
}

func (s *LiteSVM) SetDiagnosticExecution(config DiagnosticExecutionConfig) error {
	if config.PreserveRentEpoch && config.ComputeUnitLimit == 0 {
		return fmt.Errorf("%w: PreserveRentEpoch requires a diagnostic compute limit", ErrLiteSVM)
	}
	s.diagnosticExecution = config
	return nil
}

// InvocationCU records inclusive compute units in invocation completion order.
// Depth is one for top-level instructions; parent units include child units.
type InvocationCU struct {
	Program   solana.PublicKey
	Depth     uint64
	Units     uint64
	Succeeded bool
}

// RuntimeError returns the original engine error when available, or nil on
// success or when the engine supplies no underlying error. Error retains wire JSON.
func (o *TxOutcome) RuntimeError() error {
	if o == nil {
		return nil
	}
	return o.runtimeError
}

// ExecutionAccounts returns detached account snapshots before commit or rollback.
// Unlike PostAccounts, it also covers failed executions and sends.
func (o *TxOutcome) ExecutionAccounts() []PostAccount {
	if o == nil {
		return nil
	}
	return o.executionAccounts
}

func (o *TxOutcome) InvocationCU() []InvocationCU {
	if o == nil {
		return nil
	}
	return o.invocationCU
}

// ValidateProgramForExecution verifies existing deployed bytes using execution
// loader rules and the supplied feature set, without installing the program.
func ValidateProgramForExecution(programData []byte, f *FeatureSet) error {
	if f == nil {
		return fmt.Errorf("%w: ValidateProgramForExecution: nil feature set", ErrLiteSVM)
	}
	feats := f.runtimeFeatures()
	registry := sbpf.SyscallRegistry(func(u uint32) (sbpf.Syscall, bool) {
		return sealevel.Syscalls(feats, false, u)
	})
	l, err := loader.NewLoaderWithSyscalls(programData, registry, false, feats)
	if err != nil {
		return fmt.Errorf("%w: load program: %w", ErrLiteSVM, err)
	}
	program, err := l.Load()
	if err != nil {
		return fmt.Errorf("%w: load program: %w", ErrLiteSVM, err)
	}
	if err := program.Verify(); err != nil {
		return fmt.Errorf("%w: verify program: %w", ErrLiteSVM, err)
	}
	return nil
}
