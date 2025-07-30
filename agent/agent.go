package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/google/go-configfs-tsm/configfs/configfsi"
	"github.com/google/go-configfs-tsm/configfs/linuxtsm"
	tg "github.com/google/go-tdx-guest/client"
	"github.com/google/go-tpm-tools/cel"
	"github.com/google/go-tpm-tools/verifier"
	"github.com/jianlinjiang/trusted-launcher/internal/logging"
	"github.com/jianlinjiang/trusted-launcher/spec"
)

type AttestationAgent interface {
	MeasureEvent(cel.Content) error
	Attest(ctx context.Context, nonce [64]byte) ([]byte, error)
}

type attestRoot interface {
	// Extend measures the cel content into a measurement register and appends to the CEL.
	Extend(cel.Content) error
	// GetCEL fetches the CEL with events corresponding to the sequence of Extended measurements
	// to this attestation root
	GetCEL() *cel.CEL
	// Attest fetches a technology-specific quote from the root of trust.
	Attest(nonce [64]byte) (any, error)
}

type agent struct {
	measuredRots []attestRoot
	avRot        attestRoot
	launchSpec   spec.LaunchSpec
	logger       logging.Logger
}

func CreateAttestationAgent(launchSpec spec.LaunchSpec, logger logging.Logger) (AttestationAgent, error) {
	attestAgent := &agent{
		launchSpec: launchSpec,
		logger:     logger,
	}
	// check if is a TDX machine
	qp, err := tg.GetQuoteProvider()
	if err != nil {
		return nil, err
	}
	// Use qp.IsSupported to check the TDX RTMR interface is enabled
	if qp.IsSupported() == nil {
		logger.Info("Adding TDX RTMRs for measurement.")
		// try to create tsm client for tdx rtmr
		tsm, err := linuxtsm.MakeClient()
		if err != nil {
			return nil, fmt.Errorf("failed to create TSM for TDX: %v", err)
		}
		var tdxAR = &tdxAttestRoot{
			qp:        qp,
			tsmClient: tsm,
		}
		attestAgent.measuredRots = append(attestAgent.measuredRots, tdxAR)

		logger.Info("Using TDX RTMR as attestation root.")
		attestAgent.avRot = tdxAR
	} else {
		return nil, fmt.Errorf("tdx quote is not supportted")
	}

	return attestAgent, nil
}

func (a *agent) Attest(ctx context.Context, nonce [64]byte) ([]byte, error) {
	attResult, err := a.avRot.Attest(nonce)
	if err != nil {
		return nil, fmt.Errorf("failed to attest: %v", err)
	}

	var cosCel bytes.Buffer
	if err := a.avRot.GetCEL().EncodeCEL(&cosCel); err != nil {
		return nil, err
	}

	switch v := attResult.(type) {
	case *verifier.TDCCELAttestation:
		v.CanonicalEventLog = cosCel.Bytes()
		res, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal TDX CCEL attestation! %v", v)
		}
		return res, nil
	default:
		return nil, fmt.Errorf("received an unsupported attestation type! %v", v)
	}
}

// MeasureEvent takes in a cel.Content and appends it to the CEL eventlog
// under the attestation agent.
// MeasureEvent measures to all Attest Roots.
func (a *agent) MeasureEvent(event cel.Content) error {
	for _, attestRoot := range a.measuredRots {
		if err := attestRoot.Extend(event); err != nil {
			return err
		}
	}
	return nil
}

type tdxAttestRoot struct {
	tdxMu     sync.Mutex
	qp        *tg.LinuxConfigFsQuoteProvider
	tsmClient configfsi.Client
	cosCel    cel.CEL
}

func (t *tdxAttestRoot) GetCEL() *cel.CEL {
	return &t.cosCel
}

func (t *tdxAttestRoot) Extend(c cel.Content) error {
	return t.cosCel.AppendEventRTMR(t.tsmClient, cel.CosRTMR, c)
}

func (t *tdxAttestRoot) Attest(nonce [64]byte) (any, error) {
	t.tdxMu.Lock()
	defer t.tdxMu.Unlock()

	rawQuote, err := tg.GetRawQuote(t.qp, nonce)
	if err != nil {
		return nil, err
	}

	ccelData, err := os.ReadFile("/sys/firmware/acpi/tables/data/CCEL")
	if err != nil {
		return nil, err
	}
	ccelTable, err := os.ReadFile("/sys/firmware/acpi/tables/CCEL")
	if err != nil {
		return nil, err
	}

	return &verifier.TDCCELAttestation{
		CcelAcpiTable: ccelTable,
		CcelData:      ccelData,
		TdQuote:       rawQuote,
	}, nil
}
