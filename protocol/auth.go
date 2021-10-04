package protocol

const (
	CurrentVersion = 1
)

type ConnectionHeader struct {
	Version         int               `json:"VERSION"`
	Oid             string            `json:"OID"`
	InstallationKey string            `json:"IID"`
	Hostname        string            `json:"HOST_NAME,omitempty"`
	Platform        string            `json:"PLATFORM"`
	Architecture    string            `json:"ARCHITECTURE"`
	Mapping         MappingDescriptor `json:"MAPPING,omitempty"`
	SensorSeedKey   string            `json:"SENSOR_SEED_KEY"`
}
