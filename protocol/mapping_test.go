package protocol

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMappingDescriptor_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mapping MappingDescriptor
		wantErr bool
		errMsg  string
	}{
		{
			name:    "empty mapping is valid",
			mapping: MappingDescriptor{},
			wantErr: false,
		},
		{
			name: "valid ParsingRE",
			mapping: MappingDescriptor{
				ParsingRE: `(?P<timestamp>\d{4}-\d{2}-\d{2}) (?P<message>.*)`,
			},
			wantErr: false,
		},
		{
			name: "invalid ParsingRE",
			mapping: MappingDescriptor{
				ParsingRE: `(?P<timestamp>\d{4}-\d{2}-\d{2}) (?P<message>.*`,
			},
			wantErr: true,
			errMsg:  "error parsing regexp",
		},
		{
			name: "valid ParsingGrok with simple patterns",
			mapping: MappingDescriptor{
				ParsingGrok: map[string]string{
					"USERNAME": `[a-zA-Z0-9._-]+`,
					"USER":     `%{USERNAME}`,
					"message":  `User %{USER:user} logged in`,
				},
			},
			wantErr: false,
		},
		{
			name: "valid ParsingGrok with complex patterns",
			mapping: MappingDescriptor{
				ParsingGrok: map[string]string{
					"LOGLEVEL": "DEBUG|INFO|WARN|ERROR|FATAL",
					"APPNAME":  "[a-zA-Z0-9._-]+",
					"APPLOG":   `%{TIMESTAMP_ISO8601:timestamp} \[%{LOGLEVEL:level}\] \[%{APPNAME:app}\] %{GREEDYDATA:log_message}`,
					"message":  "%{APPLOG}",
				},
			},
			wantErr: false,
		},
		{
			name: "valid ParsingGrok with alternation",
			mapping: MappingDescriptor{
				ParsingGrok: map[string]string{
					"NGINX_ACCESS": `%{IPORHOST:client_ip} - %{USER:ident} \[%{HTTPDATE:timestamp}\] "%{WORD:method} %{DATA:request} HTTP/%{NUMBER:http_version}" %{INT:status} %{INT:bytes}`,
					"SYSLOG_MSG":   `%{SYSLOGTIMESTAMP:timestamp} %{SYSLOGHOST:host} %{PROG:program}: %{GREEDYDATA:log_message}`,
					"message":      "(%{NGINX_ACCESS}|%{SYSLOG_MSG})",
				},
			},
			wantErr: false,
		},
		{
			name: "invalid ParsingGrok - pattern name with colon",
			mapping: MappingDescriptor{
				ParsingGrok: map[string]string{
					"BAD:NAME": "test pattern",
					"message":  "%{BAD:NAME}",
				},
			},
			wantErr: true,
			errMsg:  "name contains unsupported character ':'",
		},
		{
			name: "invalid ParsingGrok - empty pattern name",
			mapping: MappingDescriptor{
				ParsingGrok: map[string]string{
					"": "test pattern",
				},
			},
			wantErr: false, // AddPatterns allows empty names
		},
		{
			name: "both ParsingRE and ParsingGrok valid",
			mapping: MappingDescriptor{
				ParsingRE: `(?P<timestamp>\d{4}-\d{2}-\d{2}) (?P<message>.*)`,
				ParsingGrok: map[string]string{
					"USERNAME": `[a-zA-Z0-9._-]+`,
					"message":  `User %{USERNAME:user} logged in`,
				},
			},
			wantErr: false,
		},
		{
			name: "empty ParsingGrok map is valid",
			mapping: MappingDescriptor{
				ParsingGrok: map[string]string{},
			},
			wantErr: false,
		},
		{
			name: "ParsingGrok with nested patterns",
			mapping: MappingDescriptor{
				ParsingGrok: map[string]string{
					"IPV4":       `(?:[0-9]{1,3}\.){3}[0-9]{1,3}`,
					"HOSTNAME":   `[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*`,
					"IPORHOST":   `(?:%{IPV4}|%{HOSTNAME})`,
					"NGINX_HOST": `(?:%{IPORHOST:destination.ip})(:%{INT:destination.port})?`,
					"message":    `Connection from %{NGINX_HOST}`,
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.mapping.Validate()
			if tt.wantErr {
				assert.Error(t, err, "expected validation error")
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg, "error message should contain expected text")
				}
			} else {
				assert.NoError(t, err, "expected no validation error")
			}
		})
	}
}