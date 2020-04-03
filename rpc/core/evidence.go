package core

import (
	"github.com/tendermint/tendermint/evidence"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	rpctypes "github.com/tendermint/tendermint/rpc/lib/types"
	"github.com/tendermint/tendermint/types"
)

// BroadcastEvidence broadcasts evidence of the misbehavior.
// More: https://docs.tendermint.com/master/rpc/#/Info/broadcast_evidence
func BroadcastEvidence(ctx *rpctypes.Context, ev types.Evidence) (*ctypes.ResultBroadcastEvidence, error) {
	err := evidencePool.AddEvidence(ev)
	switch err.(type) {
	case nil, evidence.ErrEvidenceAlreadyStored:
		return &ctypes.ResultBroadcastEvidence{Hash: ev.Hash()}, nil
	default:
		return nil, err
	}
}
