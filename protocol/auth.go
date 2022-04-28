package protocol

const (
	CurrentVersion = 2
)

type ConnectionHeader struct {
	Version         int               `json:"version"`
	Oid             string            `json:"oid"`
	InstallationKey string            `json:"iid"`
	Hostname        string            `json:"host_name,omitempty"`
	Platform        string            `json:"platform"`
	Architecture    string            `json:"architecture"`
	Mapping         MappingDescriptor `json:"mapping,omitempty"`
	SensorSeedKey   string            `json:"sensor_seed_key"`
	IsCompressed    bool              `json:"is_compressed"`
	DataFormat      string            `json:"data_format"` // LimaCharlie supports "msgpack" or "json"
	InstanceID      string            `json:"instance_id"`
}
