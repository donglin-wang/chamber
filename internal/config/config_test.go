package config

import (
	"reflect"
	"testing"
)

func TestOverrideFieldsMatchConfigFields(t *testing.T) {
	configType := reflect.TypeOf(Config{})
	overrideType := reflect.TypeOf(Override{})

	configFields := fieldsByName(configType)
	overrideFields := fieldsByName(overrideType)

	for name, configField := range configFields {
		overrideField, ok := overrideFields[name]
		if !ok {
			t.Fatalf("Override is missing field %s", name)
		}

		wantType := reflect.PointerTo(configField.Type)
		if overrideField.Type != wantType {
			t.Fatalf("Override.%s has type %s, want %s", name, overrideField.Type, wantType)
		}
	}

	for name := range overrideFields {
		if _, ok := configFields[name]; !ok {
			t.Fatalf("Override has extra field %s", name)
		}
	}
}

func fieldsByName(structType reflect.Type) map[string]reflect.StructField {
	fields := make(map[string]reflect.StructField, structType.NumField())
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		fields[field.Name] = field
	}
	return fields
}
