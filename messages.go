package uspclient

type UspDataMessage struct {
	SeqNum        uint64                 `json:"x"`           // populated by the client.
	AckRequested  bool                   `json:"a,omitempty"` // populated by the client.
	TextPayload   string                 `json:"t,omitempty"`
	JsonPayload   map[string]interface{} `json:"j,omitempty"`
	BinaryPayload string                 `json:"b,omitempty"`
	EventType     string                 `json:"e,omitempty"`
	Timestamp     uint64                 `json:"s,omitempty"`

	// Optional contextual information.
	ThisAtom        string `json:"this,omitempty"`
	ParentAtom      string `json:"parent,omitempty"`
	TargetAtom      string `json:"target,omitempty"`
	InvestigationID string `json:"inv,omitempty"`
}
