package protocol

import "regexp"

// Indexing descriptors are applied in the LimaCharlie cloud
// once all mapping and parsing operations have been applied.
type IndexDescriptor struct {
	// If either is specified, this descriptor only applies to
	// the specified event_types, or to all event_types excluding
	// some specific ones.
	EventsIncluded []string `json:"events_included,omitempty"`
	EventsExcluded []string `json:"events_excluded,omitempty"`

	// Path within the relevant events to use as indexed field.
	// Like: metadata/user/user_name
	Path string `json:"path"`

	// Regexp is a regular expression that applies to the element
	// in Path to extract the indexed value. It should be a regexp
	// in the `re2` format: https://github.com/google/re2/wiki/Syntax
	// with a single capture group (the value to be indexed).
	Regexp string `json:"regexp,omitempty"`

	// What index the above field belongs to. Unsupported indexes
	// will result in an error.
	// Like: user
	IndexType string `json:"index_type"`
}

func (d IndexDescriptor) Validate() error {
	if d.Regexp != "" {
		if _, err := regexp.Compile(d.Regexp); err != nil {
			return err
		}
	}
	return nil
}
