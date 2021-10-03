package protocol

type MappingDescriptor struct {
	ParsingRE    string         `json:"parsing_re,omitempty" yaml:"parsing_re,omitempty"`
	IsRenameOnly bool           `json:"rename_only" yaml:"rename_only"`
	Mappings     []FieldMapping `json:"mappings" yaml:"mappings"`
}

type FieldMapping struct {
	SourceField      string `json:"src_field" yaml:"src_field"`
	DestinationField string `json:"dst_field" yaml:"dst_field"`
}
