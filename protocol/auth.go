package protocol

const (
	CurrentVersion = 2
)

type ConnectionHeader struct {
	Version         int                 `json:"version" msgpack:"version"`
	Oid             string              `json:"oid" msgpack:"oid"`
	InstallationKey string              `json:"iid" msgpack:"iid"`
	Hostname        string              `json:"host_name,omitempty" msgpack:"host_name,omitempty"`
	Platform        string              `json:"platform" msgpack:"platform"`
	Architecture    string              `json:"architecture" msgpack:"architecture"`
	Mapping         MappingDescriptor   `json:"mapping,omitempty" msgpack:"mapping,omitempty"`
	Mappings        []MappingDescriptor `json:"mappings,omitempty" msgpack:"mappings,omitempty"`
	SensorSeedKey   string              `json:"sensor_seed_key" msgpack:"sensor_seed_key"`
	IsCompressed    bool                `json:"is_compressed" msgpack:"is_compressed"`
	DataFormat      string              `json:"data_format" msgpack:"data_format"` // LimaCharlie supports "msgpack" or "json"
	InstanceID      string              `json:"instance_id" msgpack:"instance_id"`
	Indexing        []IndexDescriptor   `json:"indexing,omitempty" msgpack:"indexing,omitempty"`
}
