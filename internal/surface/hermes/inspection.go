package hermes

import "time"

type TurnObservation struct {
	MessageID  string    `json:"message_id"`
	Status     string    `json:"status"`
	ObservedAt time.Time `json:"observed_at"`
}

type Inspection struct {
	Provider      string           `json:"provider"`
	Model         string           `json:"model"`
	Mode          string           `json:"mode"`
	ModelEvidence string           `json:"model_evidence"`
	Idle          bool             `json:"idle"`
	Turn          *TurnObservation `json:"turn,omitempty"`
}
