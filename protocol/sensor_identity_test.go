package protocol

import (
	"encoding/json"
	"github.com/vmihailenco/msgpack/v5"
	"gopkg.in/yaml.v3"
	"testing"
)

func TestSensorIdentityMappingRoundTrip(t *testing.T) {
	for _, codec := range []struct {
		data   string
		decode func([]byte, any) error
	}{
		{`{"sensor_key_path":"actor","sensor_identity_type":"github_login"}`, json.Unmarshal},
		{"sensor_key_path: actor\nsensor_identity_type: github_login\n", yaml.Unmarshal},
	} {
		var got MappingDescriptor
		if err := codec.decode([]byte(codec.data), &got); err != nil {
			t.Fatal(err)
		}
		if got.SensorIdentityType != "github_login" || got.SensorKeyPath != "actor" {
			t.Fatal("configuration dropped declaration")
		}
	}

	for _, typ := range []string{"", "email", "username", "github_login", "device"} {
		t.Run(typ, func(t *testing.T) {
			original := MappingDescriptor{SensorKeyPath: "actor", SensorIdentityType: typ}
			if err := original.Validate(); err != nil {
				t.Fatal(err)
			}
			for name, codec := range map[string]struct {
				marshal   func(any) ([]byte, error)
				unmarshal func([]byte, any) error
			}{
				"json": {json.Marshal, json.Unmarshal}, "yaml": {yaml.Marshal, yaml.Unmarshal}, "msgpack": {msgpack.Marshal, msgpack.Unmarshal},
			} {
				b, err := codec.marshal(original)
				if err != nil {
					t.Fatal(err)
				}
				var got MappingDescriptor
				if err := codec.unmarshal(b, &got); err != nil {
					t.Fatal(err)
				}
				if got.SensorIdentityType != typ || got.SensorKeyPath != "actor" {
					t.Fatalf("%s lost identity declaration: %+v", name, got)
				}
			}
		})
	}
	for _, typ := range []string{"EMAIL", "host", " email", "unknown"} {
		if (MappingDescriptor{SensorIdentityType: typ}).Validate() == nil {
			t.Fatalf("accepted invalid type %q", typ)
		}
	}
}

func TestSensorIdentityConnectionHeader(t *testing.T) {
	// A mapping must survive the actual envelope sent by an on-prem adapter,
	// including the multi-mapping spelling.
	input := ConnectionHeader{Mapping: MappingDescriptor{SensorKeyPath: "actor", SensorIdentityType: "github_login"}, Mappings: []MappingDescriptor{{SensorKeyPath: "mailbox", SensorIdentityType: "email"}}}
	for _, codec := range []struct {
		marshal   func(any) ([]byte, error)
		unmarshal func([]byte, any) error
	}{{json.Marshal, json.Unmarshal}, {msgpack.Marshal, msgpack.Unmarshal}} {
		data, err := codec.marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var got ConnectionHeader
		if err := codec.unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got.Mapping.SensorIdentityType != "github_login" || len(got.Mappings) != 1 || got.Mappings[0].SensorIdentityType != "email" {
			t.Fatal("connection header dropped sensor identity")
		}
	}
}
