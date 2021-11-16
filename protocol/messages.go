package protocol

const (
	ControlMessageACK       = "ack"
	ControlMessageBACKOFF   = "bo"
	ControlMessageRECONNECT = "re"
	ControlMessageERROR     = "error"
	ControlMessageREADY     = "rd"
)

type DataMessage struct {
	SeqNum       uint64 `json:"x" msgpack:"x"`                     // Populated by the USP Client.
	AckRequested bool   `json:"a,omitempty" msgpack:"a,omitempty"` // Populated by the USP Client.

	// One of, payload format.
	TextPayload             string                 `json:"t,omitempty" msgpack:"t,omitempty"`
	JsonPayload             map[string]interface{} `json:"j,omitempty" msgpack:"j,omitempty"`
	BinaryPayload           string                 `json:"b,omitempty" msgpack:"b,omitempty"`
	BundlePayload           []byte                 `json:"-" msgpack:"o,omitempty"` // Bundle of records
	CompressedBundlePayload []byte                 `json:"-" msgpack:"p,omitempty"` // Compressed bundle or records

	// Basic event metadata.
	EventType   string `json:"e,omitempty" msgpack:"e,omitempty"`
	TimestampMs uint64 `json:"s,omitempty" msgpack:"s,omitempty"`

	// Optional contextual information.
	ThisAtom        string `json:"this,omitempty" msgpack:"this,omitempty"`
	ParentAtom      string `json:"parent,omitempty" msgpack:"parent,omitempty"`
	TargetAtom      string `json:"target,omitempty" msgpack:"target,omitempty"`
	InvestigationID string `json:"inv,omitempty" msgpack:"inv,omitempty"`
}

type ControlMessage struct {
	Verb string `json:"v"`

	// Optional components depending on verb.
	SeqNum   uint64 `json:"x,omitempty"`
	Duration uint64 `json:"dur,omitempty"`
	Error    string `json:"err,omitempty"`
}
