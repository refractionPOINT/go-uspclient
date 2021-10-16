package protocol

const (
	CurrentVersion = 1
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
}
