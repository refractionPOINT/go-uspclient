package protocol

type MappingDescriptor struct {
	// Use the named capture fields from the regular
	// expression below to parse text lines into JSON.
	ParsingRE string `json:"parsing_re,omitempty" yaml:"parsing_re,omitempty"`

	// Path to the component of the JSON events that
	// indicates unique values to become Sensor IDs.
	SensorKeyPath string `json:"SENSOR_KEY_PATH,omitempty"`

	// Path to the component of the JSON events that
	// indicates the hostname of the sensor per the SensorKeyPath.
	SensorHostnamePath string `json:"SENSOR_HOSTNAME_PATH,omitempty"`

	// Rename the fields in the Mappings but leave
	// all other fields in place. The default behavior
	// is to replace the final event with only the
	// mapped values.
	IsRenameOnly bool           `json:"rename_only,omitempty" yaml:"rename_only,omitempty"`
	Mappings     []FieldMapping `json:"mappings,omitempty" yaml:"mappings,omitempty"`
}

type FieldMapping struct {
	// Map the source field to the destination field.
	SourceField      string `json:"src_field" yaml:"src_field"`
	DestinationField string `json:"dst_field" yaml:"dst_field"`
}
