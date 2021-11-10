package protocol

import (
	"encoding/json"
	"strconv"
)

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
	SensorKeyPath string `json:"sensor_key_path,omitempty" yaml:"sensor_key_path,omitempty"`

	// Path to the component of the JSON events that
	// indicates the hostname of the sensor per the SensorKeyPath.
	SensorHostnamePath string `json:"sensor_hostname_path,omitempty" yaml:"sensor_hostname_path,omitempty"`

	// Path to the component that should be used as
	// the Event Type of a specific event.
	EventTypePath string `json:"event_type_path,omitempty" yaml:"event_type_path,omitempty"`

	// Path to the component that should be used as
	// the Event Time (converted to a ms epoch).
	EventTimePath string `json:"event_time_path,omitempty" yaml:"event_time_path,omitempty"`

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

// This custom JSON Unmarshaler permits the `IsRenameOnly` value to be
// loaded either from a bool or a string (from an environment variable for example).
type tempMappingDescriptor MappingDescriptor

func (md *MappingDescriptor) UnmarshalJSON(data []byte) error {
	// First get all the fields parsed in a dictionary.
	d := map[string]interface{}{}
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}

	// Check if the `rename_only` field is present and if
	// it is, is it a string?
	var err error
	if ro, ok := d["rename_only"]; ok {
		if ros, ok := ro.(string); ok {
			if d["rename_only"], err = strconv.ParseBool(ros); err != nil {
				return err
			}
		}
	}

	// Re-marshal to JSON so that we can
	// do another single-pass Unmarshal.
	t, err := json.Marshal(d)
	if err != nil {
		return err
	}

	// Finally extract to a temporary type
	// (to bypass this custom Unmarshaler).
	tmd := tempMappingDescriptor{}
	if err := json.Unmarshal(t, &tmd); err != nil {
		return err
	}
	*md = MappingDescriptor(tmd)
	return nil
}
