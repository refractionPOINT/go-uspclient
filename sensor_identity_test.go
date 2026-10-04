package uspclient

import (
	"context"
	"github.com/refractionPOINT/go-uspclient/protocol"
	"strings"
	"testing"
)

func TestNewClientValidatesSensorIdentityType(t *testing.T) {
	for _, typ := range []string{"", "email", "username", "github_login", "device", "upn", "EMAIL", " email"} {
		for _, multiple := range []bool{false, true} {
			options := ClientOptions{TestSinkMode: true}
			mapping := protocol.MappingDescriptor{SensorIdentityType: typ, SensorKeyPath: "actor"}
			if multiple {
				mapping.ParsingRE = `(?P<actor>\w+)`
				options.Mappings = []protocol.MappingDescriptor{mapping}
			} else {
				options.Mapping = mapping
			}
			client, err := NewClient(context.Background(), options)
			if protocol.ValidSensorIdentityType(typ) {
				if err != nil || client == nil {
					t.Fatalf("valid declaration refused: %v", err)
				}
			} else if err == nil || client != nil || !strings.Contains(err.Error(), "sensor_identity_type") {
				t.Fatal("constructor accepted invalid declaration before test/network path")
			}
		}
	}
}
