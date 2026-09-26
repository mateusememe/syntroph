package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

var (
	// ErrSkillArgumentsSchemaInvalid reports that a package's declared
	// arguments schema is outside the approved JSON Schema subset: object,
	// string, boolean, integer, and arrays of those types, with required
	// fields, enum constraints, and size limits. Secrets are not a
	// supported argument type, so a schema requesting an unsupported type
	// (including "secret") is rejected here, before any invocation
	// argument is considered.
	ErrSkillArgumentsSchemaInvalid = errors.New("skill package declares an arguments schema outside the approved subset")
	// ErrSkillArgumentsInvalid reports that caller-supplied arguments do
	// not satisfy the package's declared arguments schema: an undeclared
	// field, a missing required field, or a type, enum, or size mismatch.
	// A package with no declared schema permits only an empty object.
	ErrSkillArgumentsInvalid = errors.New("skill invocation arguments do not satisfy the declared arguments schema")
)

// allowedSkillArgumentTypes is the approved JSON Schema subset's complete
// type vocabulary. Anything else -- including "secret", "number", or
// "null" -- is unsupported and rejected before preparation.
var allowedSkillArgumentTypes = map[string]bool{
	"object":  true,
	"string":  true,
	"boolean": true,
	"integer": true,
	"array":   true,
}

// skillArgumentSchema is the approved JSON Schema subset for skill
// arguments and every nested property or array item: object, string,
// boolean, integer, and arrays of those types, with required fields, enum
// constraints, and size limits.
type skillArgumentSchema struct {
	Type       string                         `json:"type,omitempty"`
	Properties map[string]skillArgumentSchema `json:"properties,omitempty"`
	Required   []string                       `json:"required,omitempty"`
	Items      *skillArgumentSchema           `json:"items,omitempty"`
	Enum       []any                          `json:"enum,omitempty"`
	MinLength  *int                           `json:"minLength,omitempty"`
	MaxLength  *int                           `json:"maxLength,omitempty"`
	MinItems   *int                           `json:"minItems,omitempty"`
	MaxItems   *int                           `json:"maxItems,omitempty"`
	Minimum    *float64                       `json:"minimum,omitempty"`
	Maximum    *float64                       `json:"maximum,omitempty"`
}

// parseSkillArgumentsSchema decodes a package's declared arguments schema
// strictly: unknown keywords and unsupported types fail immediately rather
// than being silently ignored. A nil or empty schema is valid and means the
// package accepts no arguments -- only an empty object is permitted.
func parseSkillArgumentsSchema(raw map[string]any) (*skillArgumentSchema, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSkillArgumentsSchemaInvalid, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var schema skillArgumentSchema
	if err := decoder.Decode(&schema); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSkillArgumentsSchemaInvalid, err)
	}
	if schema.Type == "" {
		schema.Type = "object"
	}
	if schema.Type != "object" {
		return nil, fmt.Errorf("%w: root schema type must be \"object\", got %q", ErrSkillArgumentsSchemaInvalid, schema.Type)
	}
	if err := validateSkillArgumentSchemaShape("arguments", schema); err != nil {
		return nil, err
	}
	return &schema, nil
}

// validateSkillArgumentSchemaShape confirms a (sub)schema and everything it
// nests stays inside the approved type vocabulary before any argument value
// is validated against it.
func validateSkillArgumentSchemaShape(path string, schema skillArgumentSchema) error {
	if !allowedSkillArgumentTypes[schema.Type] {
		return fmt.Errorf("%w: %s declares unsupported type %q", ErrSkillArgumentsSchemaInvalid, path, schema.Type)
	}
	switch schema.Type {
	case "array":
		if schema.Items == nil {
			return fmt.Errorf("%w: %s is an array and requires items", ErrSkillArgumentsSchemaInvalid, path)
		}
		return validateSkillArgumentSchemaShape(path+"[]", *schema.Items)
	case "object":
		for name, property := range schema.Properties {
			if err := validateSkillArgumentSchemaShape(path+"."+name, property); err != nil {
				return err
			}
		}
		return nil
	default:
		if schema.Items != nil || len(schema.Properties) != 0 {
			return fmt.Errorf("%w: %s has type %q and must not declare items or properties", ErrSkillArgumentsSchemaInvalid, path, schema.Type)
		}
		return nil
	}
}

// validateSkillArguments checks already-normalized caller-supplied
// arguments against a package's parsed schema. A nil schema -- no schema
// declared -- permits only an empty object.
func validateSkillArguments(schema *skillArgumentSchema, arguments map[string]any) error {
	if schema == nil {
		if len(arguments) != 0 {
			return fmt.Errorf("%w: this skill declares no arguments schema; only an empty object is permitted", ErrSkillArgumentsInvalid)
		}
		return nil
	}
	return validateSkillArgumentObject("arguments", *schema, arguments)
}

func validateSkillArgumentObject(path string, schema skillArgumentSchema, value map[string]any) error {
	for _, required := range schema.Required {
		if _, ok := value[required]; !ok {
			return fmt.Errorf("%w: %s.%s is required", ErrSkillArgumentsInvalid, path, required)
		}
	}
	for name, fieldValue := range value {
		property, declared := schema.Properties[name]
		if !declared {
			return fmt.Errorf("%w: %s.%s is not declared by the arguments schema", ErrSkillArgumentsInvalid, path, name)
		}
		if err := validateSkillArgumentValue(path+"."+name, property, fieldValue); err != nil {
			return err
		}
	}
	return nil
}

func validateSkillArgumentValue(path string, schema skillArgumentSchema, value any) error {
	switch schema.Type {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: %s must be an object", ErrSkillArgumentsInvalid, path)
		}
		if schema.Properties != nil {
			if err := validateSkillArgumentObject(path, schema, object); err != nil {
				return err
			}
		}
		return validateSkillArgumentEnum(path, schema.Enum, value)
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%w: %s must be a string", ErrSkillArgumentsInvalid, path)
		}
		if schema.MinLength != nil && len(text) < *schema.MinLength {
			return fmt.Errorf("%w: %s is shorter than the declared minimum length", ErrSkillArgumentsInvalid, path)
		}
		if schema.MaxLength != nil && len(text) > *schema.MaxLength {
			return fmt.Errorf("%w: %s exceeds the declared maximum length", ErrSkillArgumentsInvalid, path)
		}
		return validateSkillArgumentEnum(path, schema.Enum, value)
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%w: %s must be a boolean", ErrSkillArgumentsInvalid, path)
		}
		return validateSkillArgumentEnum(path, schema.Enum, value)
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("%w: %s must be an integer", ErrSkillArgumentsInvalid, path)
		}
		integer, err := number.Int64()
		if err != nil {
			return fmt.Errorf("%w: %s must be an integer", ErrSkillArgumentsInvalid, path)
		}
		if schema.Minimum != nil && float64(integer) < *schema.Minimum {
			return fmt.Errorf("%w: %s is less than the declared minimum", ErrSkillArgumentsInvalid, path)
		}
		if schema.Maximum != nil && float64(integer) > *schema.Maximum {
			return fmt.Errorf("%w: %s is greater than the declared maximum", ErrSkillArgumentsInvalid, path)
		}
		return validateSkillArgumentEnum(path, schema.Enum, value)
	case "array":
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%w: %s must be an array", ErrSkillArgumentsInvalid, path)
		}
		if schema.MinItems != nil && len(items) < *schema.MinItems {
			return fmt.Errorf("%w: %s has fewer than the declared minimum items", ErrSkillArgumentsInvalid, path)
		}
		if schema.MaxItems != nil && len(items) > *schema.MaxItems {
			return fmt.Errorf("%w: %s has more than the declared maximum items", ErrSkillArgumentsInvalid, path)
		}
		for i, item := range items {
			if err := validateSkillArgumentValue(fmt.Sprintf("%s[%d]", path, i), *schema.Items, item); err != nil {
				return err
			}
		}
		return validateSkillArgumentEnum(path, schema.Enum, value)
	default:
		return fmt.Errorf("%w: %s declares unsupported type %q", ErrSkillArgumentsInvalid, path, schema.Type)
	}
}

func validateSkillArgumentEnum(path string, enum []any, value any) error {
	if len(enum) == 0 {
		return nil
	}
	for _, allowed := range enum {
		if reflect.DeepEqual(allowed, value) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not one of the declared enum values", ErrSkillArgumentsInvalid, path)
}
