package protocol

// Whenever a value references a "Path", it means the value is a
// string representing where to find a specific field recursively
// in the JSON event, separated by "/".
// For example, the event:
// {
//   "some": {"my-val": "here"}
// }
// could define the Path "some/my-val" which would have the value "here".

type MappingDescriptor struct {
	// Use the named capture groups from the regular
	// expression below to parse text lines into JSON.
	ParsingRE string `json:"parsing_re,omitempty" yaml:"parsing_re,omitempty"`

	// Path to the component of the JSON events that
	// indicates unique values to become Sensor IDs.
	SensorKeyPath string `json:"SENSOR_KEY_PATH,omitempty" yaml:"sensor_key_path,omitempty"`

	// Path to the component of the JSON events that
	// indicates the hostname of the sensor per the SensorKeyPath.
	SensorHostnamePath string `json:"SENSOR_HOSTNAME_PATH,omitempty" yaml:"sensor_hostname_path,omitempty"`

	// Path to the component that should be used as
	// the Event Type of a specific event.
	EventTypePath string `json:"EVENT_TYPE_PATH,omitempty" yaml:"event_type_path,omitempty"`

	// Path to the component that should be used as
	// the Event Time (converted to a ms epoch).
	EventTimePath string `json:"EVENT_TIME_PATH,omitempty" yaml:"event_time_path,omitempty"`

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
