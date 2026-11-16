package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"
)

func TestValidateEventsPath(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		valid      bool
	}{
		{"unset", "", true},
		{"single key", "events", true},
		{"nested", "params/events", true},
		{"key with dots and spaces inside", "a.b/c d", true},
		{"max depth", strings.Repeat("k/", MaxEventsPathSegments-1) + "k", true},
		{"too deep", strings.Repeat("k/", MaxEventsPathSegments) + "k", false},
		{"leading slash", "/params/events", false},
		{"trailing slash", "params/events/", false},
		{"empty segment", "params//events", false},
		{"only slash", "/", false},
		{"whitespace only", " ", false},
		{"padded segment", "params/ events", false},
		{"trailing padded segment", "params/events\t", false},
		{"control char", "params/ev\nents", false},
		{"zero width space", "params/ev​ents", false},
		{"byte order mark", "\ufeffparams", false},
		{"invalid utf8", "params/\xff", false},
		{"max length", strings.Repeat("a", MaxEventsPathBytes), true},
		{"too long", strings.Repeat("a", MaxEventsPathBytes+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateEventsPath(tc.path)
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestMappingDescriptorEventsPath(t *testing.T) {
	t.Run("validate rejects a bad path", func(t *testing.T) {
		assert.Error(t, MappingDescriptor{EventsPath: "/bad"}.Validate())
		assert.NoError(t, MappingDescriptor{EventsPath: "params/events"}.Validate())
	})

	t.Run("json round trip", func(t *testing.T) {
		var md MappingDescriptor
		assert.NoError(t, json.Unmarshal([]byte(`{"events_path":"params/events","rename_only":"true"}`), &md))
		assert.Equal(t, "params/events", md.EventsPath)
		assert.True(t, md.IsRenameOnly)
		b, err := json.Marshal(md)
		assert.NoError(t, err)
		assert.Contains(t, string(b), `"events_path":"params/events"`)
	})

	t.Run("yaml", func(t *testing.T) {
		var md MappingDescriptor
		assert.NoError(t, yaml.Unmarshal([]byte("events_path: params/events\n"), &md))
		assert.Equal(t, "params/events", md.EventsPath)
	})

	t.Run("omitted when empty", func(t *testing.T) {
		b, err := json.Marshal(MappingDescriptor{})
		assert.NoError(t, err)
		assert.NotContains(t, string(b), "events_path")
	})
}
