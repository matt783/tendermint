package evidence

import (
	"fmt"
)

type errInvalidEvidence struct {
	Reason error
}

func (e errInvalidEvidence) Error() string {
	return fmt.Sprintf("evidence is not valid: %v ", e.Reason)
}

// ErrEvidenceAlreadyStored indicates that the evidence has already been stored in the evidence db
type ErrEvidenceAlreadyStored struct{}

func (e ErrEvidenceAlreadyStored) Error() string {
	return fmt.Sprint("evidence is already stored")
}
