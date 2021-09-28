package uspclient

const (
	uspControlMessageACK       = "ack"
	uspControlMessageBACKOFF   = "bo"
	uspControlMessageRECONNECT = "re"
	uspControlMessageERROR     = "error"
)

type UspDataMessage struct {
	SeqNum       uint64 `json:"x" msgpack:"x"`                     // Populated by the USP Client.
	AckRequested bool   `json:"a,omitempty" msgpack:"a,omitempty"` // Populated by the USP Client.

	// One of, payload format.
	TextPayload   string                 `json:"t,omitempty" msgpack:"t,omitempty"`
	JsonPayload   map[string]interface{} `json:"j,omitempty" msgpack:"j,omitempty"`
	BinaryPayload string                 `json:"b,omitempty" msgpack:"b,omitempty"`

	// Basic event metadata.
	EventType   string `json:"e,omitempty" msgpack:"e,omitempty"`
	TimestampMs uint64 `json:"s,omitempty" msgpack:"s,omitempty"`

	// Optional contextual information.
	ThisAtom        string `json:"this,omitempty" msgpack:"this,omitempty"`
	ParentAtom      string `json:"parent,omitempty" msgpack:"parent,omitempty"`
	TargetAtom      string `json:"target,omitempty" msgpack:"target,omitempty"`
	InvestigationID string `json:"inv,omitempty" msgpack:"inv,omitempty"`
}

type uspControlMessage struct {
	Verb string `json:"v" msgpack:"v"`

	// Optional components depending on verb.
	SeqNum   uint64 `json:"x,omitempty" msgpack:"x,omitempty"`
	Duration uint64 `json:"dur,omitempty" msgpack:"dur,omitempty"`
	Error    string `json:"err,omitempty" msgpack:"err,omitempty"`
}
