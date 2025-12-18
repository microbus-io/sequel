/*
Copyright (c) 2025-2026 Microbus LLC and various contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package generator

import (
	"regexp"
	"strings"

	"github.com/microbus-io/errors"
)

// Service is the top-level document for parsing service.yaml.
type Service struct {
	SQL       SQL         `yaml:"sql"`
	Functions []*Function `yaml:"functions"`
	Configs   []*Function `yaml:"configs"`
}

// SQL are the settings used to customize code generation of SQL microservices.
type SQL struct {
	Table  string `yaml:"table"`
	Object string `yaml:"object"`
}

// UnmarshalYAML parses and validates the YAML.
func (s *SQL) UnmarshalYAML(unmarshal func(any) error) error {
	// Unmarshal
	type different SQL
	var x different
	err := unmarshal(&x)
	if err != nil {
		return errors.Trace(err)
	}
	*s = SQL(x)

	// Validate
	err = s.validate()
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

// validate validates the data after unmarshaling.
func (s *SQL) validate() (err error) {
	s.Table = strings.TrimSpace(s.Table)
	if !regexp.MustCompile(`[a-z][a-z0-9_]*`).MatchString(s.Table) {
		return errors.New("invalid table name: %s", s.Table)
	}
	s.Object = strings.TrimSpace(s.Object)
	if !regexp.MustCompile(`[A-Z][A-Za-z0-9_]*`).MatchString(s.Object) {
		return errors.New("invalid object name: %s", s.Object)
	}
	return nil
}

// Function is a handler definition.
type Function struct {
	Signature   string `yaml:"signature"`
	Description string `yaml:"description"`
}

// Name extracts the name of the function from the signature.
func (fn *Function) Name() string {
	name, _, _ := strings.Cut(fn.Signature, "(")
	return strings.TrimSpace(name)
}
