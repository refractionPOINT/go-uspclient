package protocol

type ConnectionHeader struct {
	Oid             string             `json:"OID"`
	InstallationKey string             `json:"IID"`
	Hostname        string             `json:"HOST_NAME,omitempty"`
	Platform        string             `json:"PLATFORM"`
	Architecture    string             `json:"ARCHITECTURE"`
	Mapping         *MappingDescriptor `json:"MAPPING,omitempty"`
	SensorKeyPath   string             `json:"SENSOR_KEY_PATH,omitempty"`
	SensorSeedKey   string             `json:"SENSOR_SEED_KEY"`
}
