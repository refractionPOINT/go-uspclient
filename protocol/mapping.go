package protocol

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/elastic/go-grok"
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
	ParsingRE string `json:"parsing_re,omitempty" yaml:"parsing_re,omitempty" msgpack:"parsing_re,omitempty"`

	// Grok patterns to parse events where 'message' is the root field.
	ParsingGrok map[string]string `json:"parsing_grok,omitempty" yaml:"parsing_grok,omitempty" msgpack:"parsing_grok,omitempty"`

	// Path to the component of the JSON events that
	// indicates unique values to become Sensor IDs.
	SensorKeyPath string `json:"sensor_key_path,omitempty" yaml:"sensor_key_path,omitempty" msgpack:"sensor_key_path,omitempty"`

	// Path to the component of the JSON events that
	// indicates the hostname of the sensor per the SensorKeyPath.
	SensorHostnamePath string `json:"sensor_hostname_path,omitempty" yaml:"sensor_hostname_path,omitempty" msgpack:"sensor_hostname_path,omitempty"`

	// Path to the component that should be used as
	// the Event Type of a specific event.
	EventTypePath string `json:"event_type_path,omitempty" yaml:"event_type_path,omitempty" msgpack:"event_type_path,omitempty"`

	// Path to the component that should be used as
	// the Event Time (converted to a ms epoch).
	EventTimePath string `json:"event_time_path,omitempty" yaml:"event_time_path,omitempty" msgpack:"event_time_path,omitempty"`

	// Timezone to use when parsing timestamps that don't include timezone info.
	// Uses IANA timezone names (e.g., "America/New_York", "Europe/London", "UTC").
	// If not specified, timestamps without timezone info are treated as UTC.
	EventTimeTimezone string `json:"event_time_timezone,omitempty" yaml:"event_time_timezone,omitempty" msgpack:"event_time_timezone,omitempty"`

	// Path to the component that should be used as
	// the Investigation ID of a specific event.
	InvestigationIDPath string `json:"investigation_id_path,omitempty" yaml:"investigation_id_path,omitempty" msgpack:"investigation_id_path,omitempty"`

	// Rename the fields in the Mappings but leave
	// all other fields in place. The default behavior
	// is to replace the final event with only the
	// mapped values.
	// Deprecated: field mappings are now deprecated in favor of transforms.
	IsRenameOnly bool `json:"rename_only,omitempty" yaml:"rename_only,omitempty" msgpack:"rename_only,omitempty"`
	// Deprecated: field mappings are now deprecated in favor of transforms.
	Mappings []FieldMapping `json:"mappings,omitempty" yaml:"mappings,omitempty" msgpack:"mappings,omitempty"`

	// Transform applied to the events.
	Transform map[string]interface{} `json:"transform,omitempty" yaml:"transform,omitempty" msgpack:"transform,omitempty"`

	// List of field paths to drop upon ingestion.
	DropFields []string `json:"drop_fields,omitempty" yaml:"drop_fields,omitempty" msgpack:"drop_fields,omitempty"`

	// Path to a field containing a Sensor ID that already exists whithin the tenant
	// where the given event should be CCed to.
	SensorIDReplicationPath string `json:"sid_replication_path,omitempty" yaml:"sid_replication_path,omitempty" msgpack:"sid_replication_path,omitempty"`
}

// Deprecated: field mappings are now deprecated in favor of transforms.
type FieldMapping struct {
	// Map the source field to the destination field.
	SourceField      string `json:"src_field" yaml:"src_field" msgpack:"src_field"`
	DestinationField string `json:"dst_field" yaml:"dst_field" msgpack:"dst_field"`
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

func (d MappingDescriptor) Validate() error {
	if d.ParsingRE != "" {
		if _, err := regexp.Compile(d.ParsingRE); err != nil {
			return err
		}
	}
	if len(d.ParsingGrok) > 0 {
		// Use NewComplete to include all pattern sets (Syslog, Httpd, Firewalls, etc.)
		if _, err := grok.NewComplete(d.ParsingGrok); err != nil {
			return err
		}
	}
	if d.EventTimeTimezone != "" {
		if _, err := time.LoadLocation(d.EventTimeTimezone); err != nil {
			return fmt.Errorf("invalid timezone %q: %w", d.EventTimeTimezone, err)
		}
	}
	return nil
}
