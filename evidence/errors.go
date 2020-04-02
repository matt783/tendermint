package evidence

import (
	"fmt"

	"github.com/tendermint/tendermint/types"
)

type ErrInvalidEvidence struct {
	Evidence types.Evidence
	Reason   error
}

func (e ErrInvalidEvidence) Error() string {
	return fmt.Sprintf("evidence is not valid because %v: %v ", e.Reason, e.Evidence)
}

type ErrEvidenceAlreadyStored struct {
	Evidence types.Evidence
}

func (e ErrEvidenceAlreadyStored) Error() string {
	return fmt.Sprintf("evidence is already stored: %v", e.Evidence)
}
