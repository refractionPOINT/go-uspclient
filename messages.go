package uspclient

const (
	uspControlMessageACK       = "ack"
	uspControlMessageBACKOFF   = "bo"
	uspControlMessageRECONNECT = "re"
)

type UspDataMessage struct {
	SeqNum        uint64                 `json:"x"`           // populated by the client.
	AckRequested  bool                   `json:"a,omitempty"` // populated by the client.
	TextPayload   string                 `json:"t,omitempty"`
	JsonPayload   map[string]interface{} `json:"j,omitempty"`
	BinaryPayload string                 `json:"b,omitempty"`
	EventType     string                 `json:"e,omitempty"`
	TimestampMs   uint64                 `json:"s,omitempty"`

	// Optional contextual information.
	ThisAtom        string `json:"this,omitempty"`
	ParentAtom      string `json:"parent,omitempty"`
	TargetAtom      string `json:"target,omitempty"`
	InvestigationID string `json:"inv,omitempty"`
}

type uspControlMessage struct {
	Verb string `json:"v"`

	// Optional components depending on verb.
	SeqNum   uint64 `json:"x"`
	Duration uint64 `json:"dur"`
}
